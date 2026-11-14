package advisor

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

//qualifiedrefs:prose system prompt for the model, not SQL
const bloatSystemPrompt = `You are a PostgreSQL bloat remediation expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. The context reports DEAD TUPLES, not measured bloat. Dead tuples are
   reclaimed for reuse by plain VACUUM (or autovacuum); recommend plain VACUUM
   or autovacuum tuning first.
2. Never recommend VACUUM FULL or pg_repack based on the dead-tuple ratio
   alone. Only mention them as a follow-up to verify with pgstattuple when
   the table was vacuumed recently and still occupies far more space than
   its live rows need.
3. If dead tuples are shrinking or autovacuum ran recently, recommend
   "do nothing -- monitor."
4. If autovacuum has never run or dead tuples keep growing, explain the likely
   blocker (long transactions, cost limits) instead of rewriting the table.
5. For index bloat, recommend REINDEX CONCURRENTLY only with measured evidence.
6. These are ALWAYS advisory -- severity: info. Never auto-execute.

Each element: {"object_identifier":"schema.table","severity":"info",` +
	`"rationale":"...","recommended_sql":null,"bloat_pct":N,` +
	`"remediation_options":[...]}`

// bloatGUCs are the autovacuum GUCs behind the bloat advisor's first
// remedy (autovacuum tuning).
var bloatGUCs = []string{"autovacuum_vacuum_scale_factor", "autovacuum_vacuum_threshold",
	"autovacuum_vacuum_cost_limit", "autovacuum_vacuum_cost_delay", "autovacuum_max_workers"}

// bloatPromptTables is how many candidates the bloat prompt lists: the
// ones with the most estimated dead bytes (advisors-05).
const bloatPromptTables = 50

// bloatMinTrendAge is how old the window-start tables row must be for a
// trend: a row minutes old would call almost every table "stable".
const bloatMinTrendAge = time.Hour

func analyzeBloat(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}
	top, omitted := rankBloatCandidates(snap.Tables, openGateFrom(ctx), bloatPromptTables)
	if len(top) == 0 {
		return nil, nil
	}
	start, found := readBloatStart(ctx, advisorWindowFrom(ctx, cfg.Advisor.Interval()),
		snap, top, logFn)
	contexts := make([]string, 0, len(top))
	for _, t := range top {
		contexts = append(contexts, bloatTableContext(t, bloatTrend(t, start, found), start,
			found))
	}
	prompt := fmt.Sprintf("BLOAT REMEDIATION CONTEXT:\n\n"+
		"pg_repack available: %v\nDatabase size: %d bytes\n%s\n\n%s",
		slices.Contains(snap.ConfigData.ExtensionsAvailable, "pg_repack"),
		snap.System.DBSizeBytes, bloatListNote(len(top), omitted),
		strings.Join(contexts, "\n\n"))
	grounding := promptGrounding(ctx, bloatGUCs...)
	resp, err := chatAdvisor(ctx, mgr, "bloat", bloatSystemPrompt, grounding, prompt)
	if err != nil {
		return nil, err
	}
	findings := parseLLMFindings(resp, "bloat_remediation", logFn)
	for i := range findings {
		findings[i].Severity = "info"
		findings[i].RecommendedSQL = ""
	}
	return findings, nil
}

// deadBytes is a table's estimated dead bytes: its data size times its
// dead-tuple share.
func deadBytes(t collector.TableStats) float64 {
	return float64(t.TableBytes) * deadRatio(t.NLiveTup, t.NDeadTup)
}

func deadRatio(live, dead int64) float64 {
	return float64(dead) / float64(max(live+dead, 1))
}

// rankBloatCandidates is the qualifying tables without an open finding,
// the limit with the most estimated dead bytes first (ties by name), and
// how many more were left out. The collector lists tables by OID, so
// without the ranking a large catalog sent its oldest tables.
func rankBloatCandidates(tables []collector.TableStats, gate openGate, limit int) (
	[]collector.TableStats, int) {
	var out []collector.TableStats
	for _, t := range tables {
		if qualifiesForBloat(t) && !gate.hasTable(t) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := deadBytes(out[i]), deadBytes(out[j])
		if di != dj {
			return di > dj
		}
		if out[i].SchemaName != out[j].SchemaName {
			return out[i].SchemaName < out[j].SchemaName
		}
		return out[i].RelName < out[j].RelName
	})
	if len(out) <= limit {
		return out, 0
	}
	return out[:limit], len(out) - limit
}

// readBloatStart reads the listed tables' window-start counts. A missing
// source, a missing or too recent row, or a read error is no trend: the
// advice is advisory either way, so the error is logged, not returned.
func readBloatStart(ctx context.Context, w advisorWindow, snap *collector.Snapshot,
	top []collector.TableStats, logFn func(string, string, ...any)) (tablesStart, bool) {
	if w.src == nil {
		return tablesStart{}, false
	}
	keys := make([]tableKey, len(top))
	for i, t := range top {
		keys[i] = tableKey{t.SchemaName, t.RelName}
	}
	rctx, cancel := context.WithTimeout(ctx, windowHistoryTimeout)
	defer cancel()
	start, found, err := w.src.readTablesStart(rctx, w.window, keys)
	if err != nil {
		logIf(logFn, "WARN", "advisor: bloat: reading the tables history of the last %s "+
			"failed, so every dead-tuple trend is unknown: %v", w.window, err)
		return tablesStart{}, false
	}
	if !found || snap.CollectedAt.Sub(start.at) < min(bloatMinTrendAge, w.window/2) {
		return tablesStart{}, false
	}
	return start, true
}

// bloatTrend compares a table's dead-tuple share with the window start's.
func bloatTrend(t collector.TableStats, start tablesStart, found bool) string {
	past, ok := start.tables[tableKey{t.SchemaName, t.RelName}]
	if !found || !ok {
		return "unknown"
	}
	now, then := deadRatio(t.NLiveTup, t.NDeadTup), deadRatio(past.live, past.dead)
	switch {
	case now > then+0.02:
		return "growing"
	case now < then-0.02:
		return "shrinking"
	}
	return "stable"
}

func bloatTableContext(t collector.TableStats, trend string, start tablesStart,
	found bool) string {
	ratio := deadRatio(t.NLiveTup, t.NDeadTup)
	vacuumInfo := "never"
	if t.LastAutovacuum != nil {
		vacuumInfo = t.LastAutovacuum.Format("2006-01-02 15:04")
	}
	trendLabel := "Dead-tuple trend"
	if found {
		trendLabel += " since " + start.at.UTC().Format("2006-01-02 15:04 MST")
	}
	return fmt.Sprintf("Table: %s.%s\n"+
		"  Size: %.1fMB (data) + %.1fMB (indexes)\n"+
		"  Dead tuples: %d (%.1f%%)\n  Live tuples: %d\n"+
		"  Dead tuples as share of data size (not measured bloat): %.1fMB\n"+
		"  %s: %s\n  Last autovacuum: %s",
		t.SchemaName, t.RelName, float64(t.TableBytes)/mib, float64(t.IndexBytes)/mib,
		t.NDeadTup, ratio*100, t.NLiveTup, deadBytes(t)/mib, trendLabel, trend, vacuumInfo)
}

// bloatListNote says how many tables are listed and how many were left out.
func bloatListNote(listed, omitted int) string {
	if omitted == 0 {
		return fmt.Sprintf("Tables with dead tuples: %d", listed)
	}
	return fmt.Sprintf("Tables with dead tuples: %s (the %d with the most estimated "+
		"dead bytes are listed; %s omitted)", groupDigits(listed+omitted), listed,
		groupDigits(omitted))
}

// groupDigits writes n with thousands separators (4950 -> "4,950").
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
