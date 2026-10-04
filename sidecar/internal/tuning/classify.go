package tuning

import (
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Workload classification (owner decision 2), deterministic: which
// statements are the application's, a tenant family's, a test fixture's,
// diagnostic tooling or pg_sage's own, and which confirmed facts bind each
// table. The model only ever sees the result.

// StatementClass is what kind of statement a pg_stat_statements entry is.
type StatementClass string

// Statement classes. Only app and tenant statements are workload.
const (
	ClassApp        StatementClass = "app"
	ClassTenant     StatementClass = "tenant"
	ClassTest       StatementClass = "test"
	ClassDiagnostic StatementClass = "diagnostic"
	ClassSage       StatementClass = "pg_sage"
)

// StatementInfo is one statement's class, why, and the tables it reads
// or writes (canonical, sorted, deduplicated).
type StatementInfo struct {
	QueryID int64
	Class   StatementClass
	Reason  string
	Tables  []string
	Family  string
}

// TableInfo is one table's class and the routes confirmed facts give work
// on it (a source-fix packet, keep, wait, excluded).
type TableInfo struct {
	Name    string
	Class   StatementClass
	Reason  string
	Routes  []facts.Route
	FactIDs []int64
}

// Workload is the classification of one snapshot.
type Workload struct {
	Statements map[int64]StatementInfo
	Tables     map[string]TableInfo
	Counts     map[StatementClass]int
}

// IsWorkload reports an application or tenant statement.
func (w Workload) IsWorkload(id int64) bool {
	s, ok := w.Statements[id]
	return ok && (s.Class == ClassApp || s.Class == ClassTenant)
}

// ClassifyWorkload classifies snap's statements and tables. facts may hold
// any statuses: confirmed facts bind, and a rejected test-fixture fact
// overrides the test-schema name heuristic for the schemas it names.
func ClassifyWorkload(snap *collector.Snapshot, all []facts.Fact, now time.Time) Workload {
	w := Workload{Statements: map[int64]StatementInfo{}, Tables: map[string]TableInfo{},
		Counts: map[StatementClass]int{}}
	if snap == nil {
		return w
	}
	confirmed, rejectedFixtures := splitFacts(all)
	families := analyzer.CloneFamilies(snap)
	for _, t := range snap.Tables {
		info := classifyTable(t.SchemaName, t.RelName, confirmed, rejectedFixtures, now)
		if fam, ok := families[t.SchemaName]; ok && info.Class == ClassApp {
			info.Class, info.Reason = ClassTenant, "schema of tenant family "+fam
		}
		w.Tables[info.Name] = info
	}
	known := tableIndex(snap)
	for _, q := range snap.Queries {
		s := classifyStatement(q, known, w.Tables, families)
		w.Statements[q.QueryID] = s
		w.Counts[s.Class]++
	}
	return w
}

func splitFacts(all []facts.Fact) (confirmed, rejectedFixtures []facts.Fact) {
	for _, f := range all {
		switch {
		case f.Status == facts.StatusConfirmed:
			confirmed = append(confirmed, f)
		case f.Status == facts.StatusRejected && f.Type == facts.TypeTestFixture:
			rejectedFixtures = append(rejectedFixtures, f)
		}
	}
	return confirmed, rejectedFixtures
}

// classifyTable is a table's class (sage, test or app) and its routes.
func classifyTable(schema, rel string, confirmed, rejectedFixtures []facts.Fact,
	now time.Time) TableInfo {
	name := qualified(schema, rel)
	info := TableInfo{Name: name, Class: ClassApp}
	if schema == "sage" {
		info.Class, info.Reason = ClassSage, "pg_sage's own schema"
		return info
	}
	info.Routes, info.FactIDs = tableRoutes(name, confirmed, now)
	for _, b := range bindings(name, confirmed, "alter_table", now) {
		if b.Route == facts.RouteExcluded {
			info.Class = ClassTest
			info.Reason = fmt.Sprintf("confirmed test fixture (fact #%d)", b.Fact.ID)
			return info
		}
	}
	if facts.LooksLikeTestSchema(schema) && len(facts.Matching(rejectedFixtures, name)) == 0 {
		info.Class = ClassTest
		info.Reason = fmt.Sprintf("schema %s is named like a test schema", schema)
	}
	return info
}

// tableRoutes are the routes confirmed facts give a change to the table's
// definition and a drop of one of its indexes.
func tableRoutes(name string, confirmed []facts.Fact, now time.Time) ([]facts.Route,
	[]int64) {
	var routes []facts.Route
	var ids []int64
	for _, action := range []string{"alter_table", "drop_unused_index"} {
		for _, b := range bindings(name, confirmed, action, now) {
			if !slices.Contains(routes, b.Route) {
				routes = append(routes, b.Route)
			}
			if !slices.Contains(ids, b.Fact.ID) {
				ids = append(ids, b.Fact.ID)
			}
		}
	}
	return routes, ids
}

func bindings(name string, confirmed []facts.Fact, action string,
	now time.Time) []facts.Binding {
	if len(confirmed) == 0 {
		return nil
	}
	req := facts.Request{ActionType: action, Targets: []string{name}, Now: now}
	return facts.Bind(confirmed, facts.ResolveRefs(req), req)
}

// tableIndex maps a relation name to the schemas holding a table of it.
func tableIndex(snap *collector.Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, t := range snap.Tables {
		out[t.RelName] = append(out[t.RelName], t.SchemaName)
	}
	return out
}

func classifyStatement(q collector.QueryStats, known map[string][]string,
	tables map[string]TableInfo, families map[string]string) StatementInfo {
	s := StatementInfo{QueryID: q.QueryID, Class: ClassApp}
	switch reason := workload.Classify(q.Query); reason {
	case workload.Workload:
	case workload.Self:
		s.Class, s.Reason = ClassSage, "pg_sage's own statement"
		return s
	default:
		s.Class, s.Reason = ClassDiagnostic, "diagnostic tooling ("+string(reason)+")"
		return s
	}
	s.Tables = referencedTables(q.Query, known)
	for _, t := range s.Tables {
		if info := tables[t]; info.Class == ClassTest {
			s.Class, s.Reason = ClassTest, "reads "+t+": "+info.Reason
			return s
		}
	}
	for _, t := range s.Tables {
		schema, _, _ := splitQualified(t)
		if fam, ok := families[schema]; ok {
			s.Class, s.Family = ClassTenant, fam
			s.Reason = "reads " + t + " of tenant family " + fam
			return s
		}
	}
	s.Reason = "application statement"
	return s
}
