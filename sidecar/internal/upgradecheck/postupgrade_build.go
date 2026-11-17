package upgradecheck

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// The post-upgrade plan (drill-02c), built from what was read: step 1
// ANALYZE of every table pg_sage's role may analyze, then one reindex step
// per brief-listed index whose collation version still differs. What the
// role may not run, or what does not fit, is operator text.

// PostUpgradeCategory is the sage.findings category of the post-upgrade
// steps' findings (each executable plan step cites one).
const PostUpgradeCategory = "post_upgrade"

// StatisticsCarriedOver is why a plan to PostgreSQL 18 or later has no
// ANALYZE step.
const StatisticsCarriedOver = "statistics carried over"

const (
	// maxPostUpgradeTargets bounds a step's target queries.
	maxPostUpgradeTargets = 20
	// maxAnalyzeSQLRunes is the plan's SQL bound: a longer table list is a
	// database-wide ANALYZE instead.
	maxAnalyzeSQLRunes     = 10000
	analyzeDeadlineMinutes = 6 * 60
	reindexDeadlineMinutes = 24 * 60
	postUpgradeCreator     = "pg_sage post_upgrade"
	maxScopeItems          = 100
)

// LiveTable is a table of the upgraded server: its format('%I.%I') name,
// its size and whether pg_sage's role may ANALYZE it.
type LiveTable struct {
	Name        string
	Bytes       int64
	CanMaintain bool
}

// LiveIndex is an index whose collation's recorded version differs from
// the library's now, and whether pg_sage's role may rebuild it.
type LiveIndex struct {
	Index, Table string
	CanMaintain  bool
}

// postUpgradeInput is what one plan is built from.
type postUpgradeInput struct {
	Database   string
	DatabaseID *int
	Change     VersionChange
	Brief      *Brief // the stored pre-upgrade brief; nil when none
	Tables     []LiveTable
	Reindex    []LiveIndex
	Fallback   []int64 // the live top statements, when the brief names none
}

// postUpgradeFinding is the finding a step cites (same order as the steps).
type postUpgradeFinding struct {
	Ident, ObjectType, Title, Recommendation, SQL string
	Detail                                        map[string]any
}

// postUpgradeDraft is the plan before its findings are raised, with what
// pg_sage leaves to the operator and what it skipped, and why.
type postUpgradeDraft struct {
	Plan     changeplan.Plan
	Findings []postUpgradeFinding
	Operator []string
	Skipped  []string
	targets  []int64 // the ANALYZE step's
}

// originRef keys a post-upgrade plan by database and new version.
func originRef(database string, newVersion int) string {
	return "post_upgrade:" + database + ":" + strconv.Itoa(newVersion)
}

func buildPostUpgrade(in postUpgradeInput) postUpgradeDraft {
	major := in.Change.NewMajor()
	d := postUpgradeDraft{Plan: changeplan.Plan{DatabaseID: in.DatabaseID,
		Goal: fmt.Sprintf("After the upgrade to PostgreSQL %d: refresh the planner "+
			"statistics, then rebuild the indexes whose collation version changed", major),
		Origin: changeplan.OriginGenerator, OriginRef: originRef(in.Database, in.Change.New),
		Status: changeplan.StatusProposed, CreatedBy: postUpgradeCreator}}
	shares := briefShares(in.Brief)
	if major >= 18 {
		d.Skipped = append(d.Skipped, StatisticsCarriedOver+": pg_upgrade to PostgreSQL 18 "+
			"and later keeps the planner statistics (not extended statistics: ANALYZE the "+
			"tables that have CREATE STATISTICS objects)")
	} else {
		d.addAnalyze(in, shares)
	}
	d.addReindexes(in, shares)
	d.finish()
	return d
}

func briefShares(b *Brief) []TableShare {
	if b == nil {
		return nil
	}
	return b.QueryTime.Items
}

// analyzeOrder is the tables the role may ANALYZE, by the brief's share
// first, then by size (largest first) and name; denied counts the rest.
func analyzeOrder(shares []TableShare, tables []LiveTable) (ordered []string, denied int) {
	rest := slices.Clone(tables)
	slices.SortFunc(rest, func(a, b LiveTable) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.Name, b.Name))
	})
	allowed := map[string]bool{}
	for _, t := range rest {
		allowed[t.Name] = t.CanMaintain
		if !t.CanMaintain {
			denied++
		}
	}
	seen := map[string]bool{}
	for _, s := range shares {
		if allowed[s.Table] && !seen[s.Table] {
			ordered, seen[s.Table] = append(ordered, s.Table), true
		}
	}
	for _, t := range rest {
		if t.CanMaintain && !seen[t.Name] {
			ordered, seen[t.Name] = append(ordered, t.Name), true
		}
	}
	return ordered, denied
}

// targetsOf is the distinct query ids the shares of tables record, in
// share order, bounded.
func targetsOf(shares []TableShare, tables map[string]bool) []int64 {
	var out []int64
	for _, s := range shares {
		for _, id := range s.QueryIDs {
			if tables[s.Table] && len(out) < maxPostUpgradeTargets && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

func bounded(ids []int64) []int64 {
	if len(ids) > maxPostUpgradeTargets {
		return slices.Clone(ids[:maxPostUpgradeTargets])
	}
	return slices.Clone(ids)
}

func vacuumdbText(database, why string) string {
	return fmt.Sprintf("run vacuumdb --analyze-in-stages --dbname=%q as a role that may "+
		"ANALYZE every table: %s", database, why)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func (d *postUpgradeDraft) addAnalyze(in postUpgradeInput, shares []TableShare) {
	ordered, denied := analyzeOrder(shares, in.Tables)
	if denied > 0 {
		d.Operator = append(d.Operator, vacuumdbText(in.Database, plural(denied, "table")+
			" pg_sage's role may not ANALYZE"))
	}
	if len(ordered) == 0 {
		return
	}
	in2 := map[string]bool{}
	for _, t := range ordered {
		in2[t] = true
	}
	targets := targetsOf(shares, in2)
	if len(targets) == 0 {
		targets = bounded(in.Fallback)
	}
	if len(targets) == 0 {
		d.Operator = append(d.Operator, vacuumdbText(in.Database, "there are no target "+
			"queries to verify an ANALYZE step by (no stored brief and no statements yet)"))
		return
	}
	sql := "ANALYZE " + strings.Join(ordered, ", ")
	if utf8.RuneCountInString(sql) > maxAnalyzeSQLRunes {
		sql = "ANALYZE"
	}
	d.targets = targets
	d.add(analyzeStep(sql, targets), postUpgradeFinding{
		Ident:      "analyze " + in.Database + " " + strconv.Itoa(in.Change.New),
		ObjectType: "database", SQL: sql,
		Title: fmt.Sprintf("Refresh the planner statistics after the upgrade to "+
			"PostgreSQL %d", in.Change.NewMajor()),
		Recommendation: "pg_upgrade does not carry the statistics before PostgreSQL 18: " +
			"ANALYZE every table, the busiest first, before the plans regress",
		Detail: map[string]any{"tables": len(ordered), "change": in.Change.EventID,
			"order": "query_time_share then size"}})
	d.Plan.Scope.Objects = append(d.Plan.Scope.Objects, ordered...)
}

// analyzeStep is the ANALYZE step. The executor judges an ANALYZE by the
// rows modified since the last one, which pg_upgrade resets to zero, so its
// verdict after an upgrade is inconclusive by construction: an ANALYZE
// changes no data and has no rollback, so inconclusive and unverifiable
// keep it and the plan moves on; a regression still halts and asks.
func analyzeStep(sql string, targets []int64) changeplan.Step {
	return changeplan.Step{Kind: changeplan.KindAction, ActionType: "analyze_table",
		ProposedSQL: sql, TargetQueryIDs: targets, Metric: changeplan.MetricTimePerCall,
		ExpectedDirection: changeplan.DirectionDown, ExpectedSource: changeplan.SourceNone,
		DeadlineMinutes: analyzeDeadlineMinutes, OnRegressed: changeplan.ActionHaltAsk,
		OnInconclusive: changeplan.ActionKeep, OnUnverifiable: changeplan.ActionKeep}
}

func (d *postUpgradeDraft) add(s changeplan.Step, f postUpgradeFinding) {
	s.Ordinal = len(d.Plan.Steps) + 1
	d.Plan.Steps = append(d.Plan.Steps, s)
	d.Findings = append(d.Findings, f)
}

// finish adds the edges (every reindex after the ANALYZE) and the scope.
func (d *postUpgradeDraft) finish() {
	ids := map[int64]bool{}
	for _, s := range d.Plan.Steps {
		if s.Ordinal > 1 && d.Plan.Steps[0].ActionType == "analyze_table" {
			d.Plan.Edges = append(d.Plan.Edges, changeplan.Edge{From: 1, To: s.Ordinal})
		}
		for _, id := range s.TargetQueryIDs {
			if !ids[id] && len(d.Plan.Scope.QueryIDs) < maxScopeItems {
				ids[id] = true
				d.Plan.Scope.QueryIDs = append(d.Plan.Scope.QueryIDs, id)
			}
		}
	}
	if len(d.Plan.Scope.Objects) > maxScopeItems {
		d.Plan.Scope.Objects = d.Plan.Scope.Objects[:maxScopeItems]
	}
}

// addReindexes adds a reindex step for each brief-listed index whose
// collation version still differs, in brief order, within the step bound.
func (d *postUpgradeDraft) addReindexes(in postUpgradeInput, shares []TableShare) {
	if in.Brief == nil {
		return
	}
	live := map[string]LiveIndex{}
	for _, ix := range in.Reindex {
		live[ix.Index] = ix
	}
	for _, item := range in.Brief.CollationReindex.Items {
		ix, ok := live[item.Index]
		if !ok {
			continue // its collation no longer differs, or the index is gone
		}
		sql := "REINDEX INDEX CONCURRENTLY " + ix.Index
		switch {
		case !ix.CanMaintain:
			d.Operator = append(d.Operator, sql+": pg_sage's role does not own "+ix.Table+
				"; run it as its owner")
		case len(d.Plan.Steps) >= changeplan.MaxSteps:
			d.Operator = append(d.Operator, sql+": past the plan's "+
				strconv.Itoa(changeplan.MaxSteps)+" steps; run it after the plan")
		default:
			d.addReindex(in, ix, sql, d.reindexTargets(shares, ix, in.Fallback))
		}
	}
}

// reindexTargets are the statements on the index's table, else the
// ANALYZE step's, else the live top statements.
func (d *postUpgradeDraft) reindexTargets(shares []TableShare, ix LiveIndex,
	fallback []int64) []int64 {
	if t := targetsOf(shares, map[string]bool{ix.Table: true}); len(t) > 0 {
		return t
	}
	if len(d.targets) > 0 {
		return slices.Clone(d.targets)
	}
	return bounded(fallback)
}

func (d *postUpgradeDraft) addReindex(in postUpgradeInput, ix LiveIndex, sql string,
	targets []int64) {
	if len(targets) == 0 {
		d.Operator = append(d.Operator, sql+": there are no target queries to verify "+
			"it by; run it in a quiet hour")
		return
	}
	// The rollback rebuilds the index again: a reindex changes no content,
	// so undoing it is one more valid build (the engine reads it back).
	d.add(changeplan.Step{Kind: changeplan.KindAction, ActionType: ReindexAction,
		ProposedSQL: sql, RollbackSQL: sql, TargetQueryIDs: targets,
		Metric: changeplan.MetricTimePerCall, ExpectedDirection: changeplan.DirectionNone,
		ExpectedSource: changeplan.SourceNone, DeadlineMinutes: reindexDeadlineMinutes,
		OnRegressed: changeplan.ActionHaltAsk, OnUnverifiable: changeplan.ActionKeep},
		postUpgradeFinding{Ident: "reindex " + ix.Index + " " + strconv.Itoa(in.Change.New),
			ObjectType: "index", SQL: sql,
			Title: "Rebuild " + ix.Index + ": its collation version changed with the upgrade",
			Recommendation: "keys may sort differently under the new collation library; " +
				"rebuild the index, then ALTER COLLATION ... REFRESH VERSION",
			Detail: map[string]any{"table": ix.Table, "change": in.Change.EventID}})
	d.Plan.Scope.Objects = append(d.Plan.Scope.Objects, ix.Index)
}
