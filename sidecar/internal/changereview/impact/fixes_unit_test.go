package impact

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Pure parts of the review fixes: serial types, generated constraint and
// index names, the lock other statements take, in-file transaction
// opt-outs, the what-if query cap and the CR-02 floor for unknown rates.
// No concurrent-access tests: these functions are stateless.

func TestFormatTypeOfSerials(t *testing.T) {
	for in, want := range map[string]string{"serial": "integer", "serial4": "integer",
		"bigserial": "bigint", "serial8": "bigint", "smallserial": "smallint",
		"serial2": "smallint", "int8": "bigint", "varchar(20)": "character varying(20)"} {
		if got := formatType(in); got != want {
			t.Errorf("formatType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGeneratedConstraintNames(t *testing.T) {
	check := func(cols ...string) sqlast.DDLStatement {
		return sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
			Relation:   sqlast.QualifiedName{Schema: "rv", Name: "rv_orders"},
			Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintCheck, Columns: cols}}
	}
	cases := []struct {
		p    sqlast.DDLStatement
		want string
	}{
		{check("a"), "rv_orders_a_check"},
		{check("a", "b"), "rv_orders_check"},
		{check(), "rv_orders_check"},
		{sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
			Relation: sqlast.QualifiedName{Name: "rv_items"},
			Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintForeignKey,
				Columns: []string{"a", "b"}}}, "rv_items_a_b_fkey"},
		{sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
			Relation: sqlast.QualifiedName{Name: "rv_items"},
			Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintUnique,
				Columns: []string{"a", "b"}}}, "rv_items_a_b_key"},
	}
	for _, c := range cases {
		if got := constraintName(c.p); got != c.want {
			t.Errorf("constraintName(%+v) = %q, want %q", c.p.Constraint, got, c.want)
		}
	}
}

// PostgreSQL's makeObjectName keeps generated names within 63 bytes by
// shortening the longer of the table and column parts first.
func TestGeneratedNamesFitNamedatalen(t *testing.T) {
	table := strings.Repeat("t", 40)
	col := strings.Repeat("c", 30)
	fk := constraintName(sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
		Relation: sqlast.QualifiedName{Name: table},
		Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintForeignKey,
			Columns: []string{col}}})
	// 63 - len("_fkey") - 1 = 57 for table and column: the longer part loses
	// a byte at a time (the column on a tie), so 40+30 becomes 29+28.
	want := strings.Repeat("t", 29) + "_" + strings.Repeat("c", 28) + "_fkey"
	if fk != want || len(fk) != 63 {
		t.Fatalf("generated FK name %q (%d bytes), want %q", fk, len(fk), want)
	}
	idx := indexName(sqlast.DDLStatement{Kind: sqlast.DDLCreateIndex,
		Relation: sqlast.QualifiedName{Name: table},
		Index:    &sqlast.IndexDef{Columns: []string{col, "status"}}})
	if len(idx) > 63 || !strings.HasSuffix(idx, "_idx") {
		t.Fatalf("generated index name %q is %d bytes", idx, len(idx))
	}
	short := constraintName(sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
		Relation: sqlast.QualifiedName{Name: "t"}, Constraint: &sqlast.ConstraintDef{
			Kind: sqlast.ConstraintPrimaryKey}})
	if short != "t_pkey" {
		t.Fatalf("primary key name %q", short)
	}
}

func TestStatementLockOfOtherStatements(t *testing.T) {
	cases := []struct {
		p    sqlast.DDLStatement
		want string
	}{
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "TruncateStmt"},
			lockAccessExclusive},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "LockStmt",
			TableLock: "SHARE"}, lockShare},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "LockStmt",
			TableLock: "ACCESS SHARE"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "LockStmt",
			TableLock: "EXCLUSIVE"}, lockExclusive},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "CommentStmt"}, ""},
	}
	for _, c := range cases {
		if got := statementLock(c.p); got != c.want {
			t.Errorf("statementLock(%s %s) = %q, want %q", c.p.Command, c.p.TableLock, got,
				c.want)
		}
	}
}

func TestUnmodeledSubjects(t *testing.T) {
	cases := []struct {
		p    sqlast.DDLStatement
		want string
	}{
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "CommentStmt"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "SelectStmt"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "TruncateStmt"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "LockStmt"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "AlterTableStmt",
			Subcommand: "AT_SetRelOptions"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "AlterTableStmt",
			Subcommand: "AT_SetUnLogged"}, "ALTER TABLE SetUnLogged"},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "DropStmt",
			ObjectType: "function"}, "DROP FUNCTION"},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "DropStmt",
			ObjectType: "schema"}, ""},
		{sqlast.DDLStatement{Kind: sqlast.DDLOther, Command: "CreateTrigStmt"},
			"create_trig statement"},
		{sqlast.DDLStatement{Kind: sqlast.DDLAddColumn}, ""},
	}
	for _, c := range cases {
		if got := unmodeledSubject(c.p); got != c.want {
			t.Errorf("unmodeledSubject(%s %s %s) = %q, want %q", c.p.Command,
				c.p.Subcommand, c.p.ObjectType, got, c.want)
		}
	}
}

func TestTransactionOptOutDirectives(t *testing.T) {
	cases := []struct {
		tool, content string
		line          int
		want          bool
	}{
		{"goose", "-- +goose Up\n-- +goose NO TRANSACTION\nCREATE INDEX ...", 3, true},
		{"goose", "-- +goose Up\nCREATE INDEX ...", 2, false},
		{"goose", "--   +goose   no transaction\nX", 2, true},
		{"dbmate", "-- migrate:up transaction:false\nA\n-- migrate:down\nB", 2, true},
		{"dbmate", "-- migrate:up transaction:false\nA\n-- migrate:down\nB", 4, false},
		{"dbmate", "-- migrate:up\nA\n-- migrate:down transaction:false\nB", 4, true},
		{"liquibase", "--changeset a:1 runInTransaction:false\nA\n--changeset a:2\nB", 2,
			true},
		{"liquibase", "--changeset a:1 runInTransaction:false\nA\n--changeset a:2\nB", 4,
			false},
		{"atlas", "-- atlas:txmode none\nA", 2, true},
		{"flyway", "-- +goose NO TRANSACTION\nA", 2, false},
		{"", "-- +goose NO TRANSACTION\nA", 2, false},
		{"goose", "", 1, false},
	}
	for _, c := range cases {
		if got := txnOptOut(c.tool, c.content)(c.line); got != c.want {
			t.Errorf("%s %q line %d: opted out %v, want %v", c.tool, c.content, c.line,
				got, c.want)
		}
	}
}

// whatIfQueries measures at most maxWhatIfQueries workload reads, the
// busiest first and one per fingerprint.
func TestWhatIfQueriesAreBounded(t *testing.T) {
	rel := &relation{name: "rv.t", schema: "rv", rel: "t", found: true}
	w := &workload{available: true, byRelation: map[string][]*workStmt{}}
	for i := 0; i < 3*maxWhatIfQueries; i++ {
		ws := &workStmt{id: int64(1000 + i), text: fmt.Sprintf("SELECT %d", i),
			calls: int64(i), fingerprint: fmt.Sprintf("fp%d", i%(2*maxWhatIfQueries)),
			refs: sqlast.Refs{Kind: sqlast.StmtSelect}}
		w.byRelation[rel.name] = append(w.byRelation[rel.name], ws)
	}
	r := &run{work: w}
	qs, ids := r.whatIfQueries(rel)
	if len(qs) != maxWhatIfQueries || len(ids) != maxWhatIfQueries {
		t.Fatalf("%d queries (%d ids), want %d", len(qs), len(ids), maxWhatIfQueries)
	}
	seen := map[string]bool{}
	for i, q := range qs {
		if i > 0 && qs[i-1].Calls < q.Calls {
			t.Fatalf("queries not busiest first: %d calls before %d", qs[i-1].Calls, q.Calls)
		}
		fp := fmt.Sprintf("fp%d", (q.QueryID-1000)%int64(2*maxWhatIfQueries))
		if seen[fp] {
			t.Fatalf("fingerprint %s measured twice", fp)
		}
		seen[fp] = true
	}
	if qs[0].Calls != int64(3*maxWhatIfQueries-1) {
		t.Fatalf("busiest query has %d calls", qs[0].Calls)
	}
}

// A lock queue whose conflicting statements all lack a rate is not "about
// 0 statements" at info severity: it is at least a warning.
func TestLockQueueWithUnknownRatesIsAWarning(t *testing.T) {
	rel := &relation{name: "rv.t", schema: "rv", rel: "t", found: true, evID: "table:rv.t",
		columns: map[string]column{}, failed: map[string]string{}}
	ws := &workStmt{id: 7, text: "SELECT 1 FROM rv.t", calls: 100,
		refs: sqlast.Refs{Kind: sqlast.StmtSelect, LockMode: sqlast.LockAccessShare,
			Relations: []sqlast.QualifiedName{{Schema: "rv", Name: "t"}}}}
	r := &run{e: &Engine{d: Deps{Logf: func(string, ...any) {}}}, ctx: context.Background(),
		cfg: config.DefaultChangeReviewConfig(), rels: map[string]*relation{rel.name: rel},
		evIdx: map[string]int{}, unknown: map[string]string{},
		work: &workload{available: true, stmts: []*workStmt{ws},
			byRelation: map[string][]*workStmt{rel.name: {ws}}}}
	s := &stmt{index: 1, evID: "stmt:1", sql: "ALTER TABLE rv.t ALTER COLUMN a TYPE bigint",
		parts: []sqlast.DDLStatement{{Kind: sqlast.DDLAlterType, Relation: sqlast.QualifiedName{
			Schema: "rv", Name: "t"}, Column: &sqlast.ColumnDef{Name: "a", Type: "int8"}}},
		targets: []string{rel.name}}
	as := []assessment{{risk: &migration.DDLRisk{LockLevel: lockAccessExclusive,
		EstimatedLockMs: 3000}}}
	r.lockQueue(s, 0, as)
	if len(r.finds) != 1 || r.finds[0].RuleID != RuleLockQueue {
		t.Fatalf("findings %+v, want one CR-02", r.finds)
	}
	f := r.finds[0]
	if f.Severity != changereview.SeverityWarning || !strings.Contains(f.Text, "unknown") {
		t.Fatalf("CR-02 %s %q, want a warning saying the rate is unknown", f.Severity, f.Text)
	}
	ws.rateKnown, ws.rate = true, queryRate{peak: 0.01}
	r.finds = nil
	r.lockQueue(s, 0, as)
	if r.finds[0].Severity != changereview.SeverityInfo {
		t.Fatalf("a known, low rate: CR-02 %s, want info", r.finds[0].Severity)
	}
}
