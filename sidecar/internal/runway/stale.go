package runway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// openSlotsSQL lists the slots with an open WAL forecast finding.
const openSlotsSQL = `/* pg_sage */ SELECT substr(object_identifier, 6)
	FROM sage.findings
	WHERE category = $1 AND status = 'open' AND object_identifier LIKE 'slot:%'
	ORDER BY 1`

// fillOpenSlots reads, while the slots are unreadable, the slots with an
// open finding, so Holds keeps those without samples open. Otherwise (or
// on a standby, which writes nothing) it reads nothing.
func (m *Monitor) fillOpenSlots(ctx context.Context, in *EvalInput, standby bool) error {
	if standby || !in.TrendsOK || in.SlotsOK {
		return nil
	}
	rows, err := m.pool.Query(ctx, openSlotsSQL, CategoryWAL)
	if err != nil {
		return fmt.Errorf("list the open slot findings (slots unreadable): %w", err)
	}
	in.OpenSlots, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list the open slot findings (slots unreadable): %w", err)
	}
	return nil
}

// without is cats less cat.
func without(cats []string, cat string) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		if c != cat {
			out = append(out, c)
		}
	}
	return out
}

// markStaleSQL marks the held open findings stale in one statement. It
// writes only a finding not already stale for that reason, keeps the
// first stale_since, and returns what it marked (to log each transition
// once). The next measured upsert replaces the detail, which clears it.
const markStaleSQL = `/* pg_sage */ UPDATE sage.findings f
	SET detail = CASE WHEN jsonb_typeof(f.detail) = 'object' THEN f.detail
	                  ELSE '{}'::jsonb END
	    || jsonb_build_object('stale', true, 'stale_reason', h.reason,
	           'stale_since', COALESCE(f.detail -> 'stale_since', to_jsonb(now())))
	FROM unnest($1::text[], $2::text[], $3::text[]) AS h(category, ident, reason)
	WHERE f.category = h.category AND f.object_identifier = h.ident
	  AND f.status = 'open'
	  AND f.detail ->> 'stale_reason' IS DISTINCT FROM h.reason
	RETURNING f.category, f.object_identifier, h.reason`

// keepOpen is the identifiers of cat that must not resolve: its active
// runways and its held series.
func keepOpen(active map[string]map[string]bool, held map[string]map[string]string,
	cat string) map[string]bool {
	keep := make(map[string]bool, len(active[cat])+len(held[cat]))
	for ident := range active[cat] {
		keep[ident] = true
	}
	for ident := range held[cat] {
		keep[ident] = true
	}
	return keep
}

// markStale marks the held open findings stale and logs each finding
// that becomes stale, or stops being held, once. Nil holds (unreadable
// trends: nothing was measured or resolved) leave the marks alone.
func (m *Monitor) markStale(ctx context.Context, held map[string]map[string]string) error {
	if held == nil {
		return nil
	}
	m.staleMu.Lock()
	defer m.staleMu.Unlock()
	m.releaseUnheld(held)
	var cats, idents, reasons []string
	for _, cat := range sortedKeys(held) {
		for _, ident := range sortedKeys(held[cat]) {
			cats, idents = append(cats, cat), append(idents, ident)
			reasons = append(reasons, held[cat][ident])
		}
	}
	if len(cats) == 0 {
		return nil
	}
	rows, err := m.pool.Query(ctx, markStaleSQL, cats, idents, reasons)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cat, ident, reason string
		if err := rows.Scan(&cat, &ident, &reason); err != nil {
			return err
		}
		m.stale[cat+"\x00"+ident] = true
		m.logFn("INFO", "runway: db %q: %s finding %s held open, marked stale: its "+
			"series is %s this tick, which is not a cleared runway", m.opts.Database,
			cat, ident, reason)
	}
	return rows.Err()
}

// releaseUnheld forgets (and logs) the stale findings no longer held:
// measured again (the upsert cleared the mark) or free to resolve.
func (m *Monitor) releaseUnheld(held map[string]map[string]string) {
	for key := range m.stale {
		cat, ident, _ := strings.Cut(key, "\x00")
		if _, ok := held[cat][ident]; ok {
			continue
		}
		delete(m.stale, key)
		m.logFn("INFO", "runway: db %q: %s finding %s is no longer held stale",
			m.opts.Database, cat, ident)
	}
}

// noteTruncation logs once when the trends read starts or stops being
// cut at its row cap.
func (m *Monitor) noteTruncation(set probes.RunwayTrendSet) {
	m.staleMu.Lock()
	defer m.staleMu.Unlock()
	if set.Truncated == m.truncated {
		return
	}
	m.truncated = set.Truncated
	if set.Truncated {
		m.logFn("WARN", "runway: db %q: runway_trends reached its row cap after %s/%s; "+
			"findings of the series it may have cut are not resolved until it fits",
			m.opts.Database, set.LastKind, set.LastSubject)
		return
	}
	m.logFn("INFO", "runway: db %q: runway_trends fits its row cap again", m.opts.Database)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
