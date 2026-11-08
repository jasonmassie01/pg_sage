package advisor

import (
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// Absence streaks after which an open advisory finding resolves: a
// rewritten (or retired) query must stay out of pg_stat_statements for
// rewriteAbsentCycles cycles, a table must stay under the bloat floor for
// bloatClearCycles cycles, so one quiet snapshot never resolves either.
const (
	rewriteAbsentCycles = 3
	bloatClearCycles    = 2
)

// resolution is the deterministic re-check of the open advisor findings
// on one snapshot (advisors-02): what stays open (re-emitted unchanged,
// as the tuning agent's keepOpen does), the objects it gates out of this
// cycle's prompts, and the categories the pass fully read. No model is
// in the loop: a finding resolves only on evidence, never because the
// model went quiet.
type resolution struct {
	kept      []analyzer.Finding
	gate      openGate
	evaluated map[string]bool
}

// resolveOpen re-checks every open advisor finding against snap.
func (a *Advisor) resolveOpen(snap *collector.Snapshot, open []analyzer.Finding) resolution {
	res := resolution{gate: newOpenGate(open), evaluated: readableCategories(snap)}
	live := map[string]bool{}
	for _, f := range open {
		key := f.Category + "|" + f.ObjectIdentifier
		live[key] = true
		if why := a.resolvedBy(snap, f, key, res.evaluated[f.Category]); why != "" {
			a.logFn("INFO", "advisor: open finding %s %s resolves: %s", f.Category,
				f.ObjectIdentifier, why)
			continue
		}
		res.kept = append(res.kept, f)
	}
	a.streaks.forgetExcept(live)
	return res
}

// readableCategories are the categories whose evidence snap carries.
func readableCategories(snap *collector.Snapshot) map[string]bool {
	settings := snap.ConfigData != nil && len(snap.ConfigData.PGSettings) > 0
	return map[string]bool{
		"wal_tuning":        settings,
		"connection_tuning": settings,
		"query_rewrite": snap.Available("queries") && !snap.StatsReset &&
			len(snap.Queries) > 0,
		"bloat_remediation": snap.Available("tables") && len(snap.Tables) > 0,
	}
}

// resolvedBy returns why f no longer holds, or "" to keep it open. An
// unreadable category keeps every finding.
func (a *Advisor) resolvedBy(snap *collector.Snapshot, f analyzer.Finding, key string,
	readable bool) string {
	if !readable {
		return ""
	}
	switch f.Category {
	case "wal_tuning", "connection_tuning":
		return settingApplied(snap.ConfigData.PGSettings, f)
	case "query_rewrite":
		return a.rewriteRetired(snap, f, key)
	case "bloat_remediation":
		return a.bloatCleared(snap, f, key)
	}
	return ""
}

// settingApplied reports a setting finding whose recommended value is the
// live value (a DBA applied it by hand, or the provider did).
func settingApplied(settings []collector.PGSetting, f analyzer.Finding) string {
	name, want, ok := findingSetting(f)
	if !ok {
		return ""
	}
	for _, s := range settings {
		if s.Name == name && settingEquals(want, s) {
			return "live " + name + " is the recommended " + want
		}
	}
	return ""
}

// findingSetting is the setting and value a finding recommends: from its
// ALTER SYSTEM, its managed ALTER DATABASE, or its managed-change intent.
func findingSetting(f analyzer.Finding) (name, value string, ok bool) {
	if st, ok := pgconf.ParseAlterSystem(f.RecommendedSQL); ok && !st.Reset {
		return st.Name, st.Value, true
	}
	if st, ok := pgconf.ParseAlterDatabase(f.RecommendedSQL); ok && !st.Reset {
		return st.Name, st.Value, true
	}
	if in, ok := managedparam.IntentFromDetail(f.Detail); ok {
		return in.Parameter, in.Value, true
	}
	return "", "", false
}

// findingGUC is the setting a finding is about, even one without a value
// (advisory guidance): its statement, or the "instance:<guc>" identifier.
func findingGUC(f analyzer.Finding) string {
	if name, _, ok := findingSetting(f); ok {
		return name
	}
	if _, guc, found := strings.Cut(f.ObjectIdentifier, ":"); found {
		return strings.ToLower(strings.TrimSpace(guc))
	}
	return ""
}

// settingEquals compares a recommended value with a live pg_settings row,
// unit-normalized: "4GB" equals setting 4096 with unit MB. A setting
// waiting for a restart is not live yet.
func settingEquals(want string, s collector.PGSetting) bool {
	if s.PendingRestart {
		return false
	}
	if w, err := pgconf.ToBaseUnits(want, s.Unit); err == nil {
		if live, err := strconv.ParseFloat(strings.TrimSpace(s.Setting), 64); err == nil {
			return math.Abs(w-live) <= 1e-9*math.Max(1, math.Abs(live))
		}
	}
	return normalizeEnum(want) == normalizeEnum(s.Setting)
}

// normalizeEnum folds case, quotes and PostgreSQL's boolean spellings.
func normalizeEnum(v string) string {
	s := strings.ToLower(strings.TrimSpace(pgconf.Unquote(strings.TrimSpace(v))))
	switch s {
	case "on", "true", "yes", "1":
		return "on"
	case "off", "false", "no", "0":
		return "off"
	}
	return s
}

// rewriteRetired resolves a rewrite whose query has been absent from
// pg_stat_statements for rewriteAbsentCycles consecutive cycles.
func (a *Advisor) rewriteRetired(snap *collector.Snapshot, f analyzer.Finding,
	key string) string {
	qid, ok := rewriteQueryID(f.ObjectIdentifier)
	if !ok {
		return ""
	}
	present := false
	for _, q := range snap.Queries {
		if q.QueryID == qid {
			present = true
			break
		}
	}
	if n := a.streaks.observe(key, snap, !present); n >= rewriteAbsentCycles {
		return "queryid absent from pg_stat_statements for " + strconv.Itoa(n) + " cycles"
	}
	return ""
}

// rewriteQueryID parses the "queryid:NNN" identifier of a rewrite.
func rewriteQueryID(ident string) (int64, bool) {
	after, found := strings.CutPrefix(ident, "queryid:")
	if !found {
		return 0, false
	}
	qid, err := strconv.ParseInt(strings.TrimSpace(after), 10, 64)
	return qid, err == nil
}

// bloatCleared resolves a bloat finding whose table has been under the
// bloat advisor's own floor (10% dead, 1000 rows) for bloatClearCycles
// cycles. A table the snapshot does not name keeps its finding.
func (a *Advisor) bloatCleared(snap *collector.Snapshot, f analyzer.Finding,
	key string) string {
	want := canonicalTable(f.ObjectIdentifier)
	for _, t := range snap.Tables {
		if canonicalTable(t.SchemaName+"."+t.RelName) != want {
			continue
		}
		n := a.streaks.observe(key, snap, !qualifiesForBloat(t))
		if n >= bloatClearCycles {
			return "dead tuples under the bloat floor for " + strconv.Itoa(n) + " cycles"
		}
		return ""
	}
	return ""
}

// clearStreaks counts, per open finding, consecutive cycles whose evidence
// cleared it. A snapshot is counted once, however often it is re-checked.
// Streaks live in memory: a restart starts them over, which only delays
// a resolve.
type clearStreaks struct {
	mu sync.Mutex
	m  map[string]clearStreak
}

type clearStreak struct {
	n    int
	snap *collector.Snapshot
}

func (c *clearStreaks) observe(key string, snap *collector.Snapshot, cleared bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]clearStreak{}
	}
	s := c.m[key]
	if s.snap == snap {
		return s.n
	}
	s.snap = snap
	if cleared {
		s.n++
	} else {
		s.n = 0
	}
	c.m[key] = s
	return s.n
}

// forgetExcept drops the streaks of findings no longer open.
func (c *clearStreaks) forgetExcept(open map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if !open[k] {
			delete(c.m, k)
		}
	}
}
