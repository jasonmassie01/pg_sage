package analyzer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// cloneSignals is the evidence that decides whether a clone family is
// idle (a leftover) or live (Phase 0 item 10).
type cloneSignals struct {
	// sessionSchemas are schemas some backend holds a relation lock in;
	// nil means the lookup failed, which is not evidence of idleness.
	sessionSchemas map[string]bool
	// sessionQueries are the (lower-cased) statements of client sessions.
	sessionQueries []string
	now            time.Time
	// window is how long a family's counters must stay unchanged before
	// activity since the statistics reset no longer counts.
	window  time.Duration
	tracker *cloneTracker
}

// cloneTracker remembers, per family, its activity signature (summed
// scan and tuple counters) and since when it has not changed. It lives
// on the analyzer and is touched only by the cycle goroutine.
type cloneTracker struct {
	marks map[string]cloneMark
}

type cloneMark struct {
	sig   int64
	since time.Time
}

func newCloneTracker() *cloneTracker {
	return &cloneTracker{marks: map[string]cloneMark{}}
}

// quietFor records sig for key and returns how long it has been
// unchanged (zero on first sight or after a change).
func (t *cloneTracker) quietFor(key string, sig int64, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	mark, ok := t.marks[key]
	if !ok || mark.sig != sig {
		t.marks[key] = cloneMark{sig: sig, since: now}
		return 0
	}
	return now.Sub(mark.since)
}

// familySignature sums the members' scan and tuple counters (cumulative
// since the statistics reset).
func familySignature(snap *collector.Snapshot, members []string) int64 {
	in := make(map[string]bool, len(members))
	for _, m := range members {
		in[m] = true
	}
	var sig int64
	for _, t := range snap.Tables {
		if in[t.SchemaName] {
			sig += t.SeqScan + t.IdxScan + t.NTupIns + t.NTupUpd + t.NTupDel
		}
	}
	return sig
}

// familyIdle reports whether a family is idle, with the evidence. Idle
// needs all of: no scan/tuple activity since the statistics reset (or
// counters unchanged for the whole window), no pg_stat_statements text
// naming a member, and a known set of sessions none of which uses one.
func familyIdle(snap *collector.Snapshot, key string, members []string,
	sig cloneSignals) (bool, string) {
	counters := familySignature(snap, members)
	quiet := sig.tracker.quietFor(key, counters, sig.now)
	if counters > 0 && quiet < sig.window {
		return false, "tables were scanned or written within the idle window"
	}
	for _, q := range snap.Queries {
		if m := referencedSchema(strings.ToLower(q.Query), members); m != "" {
			return false, "pg_stat_statements has statements on " + m
		}
	}
	if sig.sessionSchemas == nil {
		return false, "session activity is unknown"
	}
	for _, m := range members {
		if sig.sessionSchemas[m] {
			return false, "a session holds locks in " + m
		}
	}
	for _, q := range sig.sessionQueries {
		if m := referencedSchema(q, members); m != "" {
			return false, "a session runs statements on " + m
		}
	}
	return true, "no scans or writes since the statistics reset or within the " +
		"idle window, and no statement or session uses them"
}

// referencedSchema returns the first member that lowerText qualifies an
// object with (schema. or "schema".), or "".
func referencedSchema(lowerText string, members []string) string {
	for _, m := range members {
		lm := strings.ToLower(m)
		if strings.Contains(lowerText, lm+".") || strings.Contains(lowerText, `"`+lm+`".`) {
			return m
		}
	}
	return ""
}

// maxAffectedSchemas caps the schema names listed on one fanned-out issue.
const maxAffectedSchemas = 100

// fanOutFamily reports each distinct issue of a live family once: the
// most severe instance (then the first schema) is kept as is — its SQL
// fixes its own schema — and carries the list of affected schemas.
func fanOutFamily(key string, findings []Finding) []Finding {
	groups := map[string][]Finding{}
	for _, f := range findings {
		_, rest, _ := strings.Cut(f.ObjectIdentifier, ".")
		id := f.Category + "\x1f" + f.ObjectType + "\x1f" + rest
		groups[id] = append(groups[id], f)
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Finding, 0, len(ids))
	for _, id := range ids {
		out = append(out, fanOutIssue(key, groups[id]))
	}
	return out
}

func fanOutIssue(key string, group []Finding) Finding {
	sort.SliceStable(group, func(i, j int) bool {
		ri, rj := severityRank(group[i].Severity), severityRank(group[j].Severity)
		if ri != rj {
			return ri > rj
		}
		return group[i].ObjectIdentifier < group[j].ObjectIdentifier
	})
	seen := map[string]bool{}
	var schemas []string
	for _, f := range group {
		schema, _, _ := strings.Cut(f.ObjectIdentifier, ".")
		if !seen[schema] {
			seen[schema] = true
			schemas = append(schemas, schema)
		}
	}
	sort.Strings(schemas)
	rep := group[0]
	detail := make(map[string]any, len(rep.Detail)+3)
	for k, v := range rep.Detail {
		detail[k] = v
	}
	detail["schema_family"] = key
	detail["affected_schema_count"] = len(schemas)
	if len(schemas) > maxAffectedSchemas {
		schemas = schemas[:maxAffectedSchemas]
	}
	detail["affected_schemas"] = schemas
	rep.Detail = detail
	if n := len(seen); n > 1 {
		rep.Title += fmt.Sprintf(" (and %d more schemas in this schema family)", n-1)
	}
	return rep
}

// cloneSessionsSQL lists the schemas other backends of this database hold
// relation locks in, and the statement text of its client sessions.
// pg_sage's own sessions are left out (its collector locks every sequence
// it reads, clone schemas' included).
var cloneSessionsSQL = `/* pg_sage */
SELECT DISTINCT n.nspname, NULL::text
  FROM pg_catalog.pg_locks l
  JOIN pg_catalog.pg_class c ON c.oid = l.relation
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_catalog.pg_stat_activity la ON la.pid = l.pid
 WHERE l.database = (SELECT oid FROM pg_catalog.pg_database
                      WHERE datname = current_database())
   AND l.pid <> pg_backend_pid()
   AND ` + selfmonitor.ActivityExclusionSQL("la") + `
UNION ALL
SELECT NULL, left(lower(a.query), 4096)
  FROM pg_catalog.pg_stat_activity a
 WHERE a.datname = current_database() AND a.pid <> pg_backend_pid()
   AND a.backend_type = 'client backend' AND COALESCE(a.query, '') <> ''
   AND ` + selfmonitor.ActivityExclusionSQL("a")

// loadCloneSessions returns the schemas locked by other sessions and the
// sessions' statements. On error both are nil (unknown).
func (a *Analyzer) loadCloneSessions(ctx context.Context) (map[string]bool, []string, error) {
	rows, err := a.catalog().Query(ctx, cloneSessionsSQL)
	if err != nil {
		return nil, nil, fmt.Errorf("load sessions for clone families: %w", err)
	}
	defer rows.Close()
	schemas := map[string]bool{}
	var queries []string
	for rows.Next() {
		var schema, query *string
		if err := rows.Scan(&schema, &query); err != nil {
			return nil, nil, fmt.Errorf("scan clone-family session: %w", err)
		}
		if schema != nil {
			schemas[*schema] = true
		}
		if query != nil {
			queries = append(queries, *query)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate clone-family sessions: %w", err)
	}
	return schemas, queries, nil
}

// cloneSignals gathers this cycle's evidence for collapseCloneSchemas. A
// failed session lookup leaves sessions unknown, so no family is called
// a leftover this cycle.
func (a *Analyzer) cloneSignals(ctx context.Context) cloneSignals {
	if a.cloneTracker == nil {
		a.cloneTracker = newCloneTracker()
	}
	days := a.cfg.Analyzer.UnusedIndexWindowDays
	if days <= 0 {
		days = 7
	}
	sig := cloneSignals{now: time.Now(), tracker: a.cloneTracker,
		window: time.Duration(days) * 24 * time.Hour}
	schemas, queries, err := a.loadCloneSessions(ctx)
	if err != nil {
		a.logFn("WARN", "analyzer: %v", err)
		return sig
	}
	sig.sessionSchemas, sig.sessionQueries = schemas, queries
	return sig
}
