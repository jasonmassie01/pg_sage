package executor

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// review-02c: set_column_statistics, a moderate reversible contract for one
// ALTER COLUMN ... SET STATISTICS clause. Every other ALTER TABLE stays the
// high-risk alter_table.

func TestColumnStatistics_ClassifierTable(t *testing.T) {
	cases := []struct {
		sql, want string
	}{
		{"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 1000",
			ActionTypeSetColumnStatistics},
		{"alter table public.t alter column c set statistics 1000;",
			ActionTypeSetColumnStatistics},
		{"ALTER TABLE public.t ALTER c SET STATISTICS 10000", ActionTypeSetColumnStatistics},
		{`ALTER TABLE "public"."T" ALTER COLUMN "we""ird;col" SET STATISTICS 100`,
			ActionTypeSetColumnStatistics},
		{"ALTER TABLE ONLY public.t ALTER COLUMN c SET STATISTICS -1",
			ActionTypeSetColumnStatistics},
		// Out of range still has the shape; the validator refuses it.
		{"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 0", ActionTypeSetColumnStatistics},
		{"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 100, DROP COLUMN x", "alter_table"},
		{"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 100; DROP TABLE t", "alter_table"},
		{"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS DEFAULT", "alter_table"},
		{"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 1e3", "alter_table"},
		{"ALTER TABLE public.t ALTER COLUMN c TYPE bigint", "alter_table"},
		{"ALTER TABLE public.t ALTER COLUMN c SET (n_distinct = 100)", "alter_table"},
		{"ALTER TABLE public.t DROP COLUMN c", "alter_table"},
		{"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)",
			"set_table_autovacuum"},
	}
	for _, tc := range cases {
		if got := ActionTypeForSQL(tc.sql); got != tc.want {
			t.Errorf("ActionTypeForSQL(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

func TestColumnStatistics_ValidatorRange(t *testing.T) {
	for _, n := range []string{"1", "100", "10000", "-1"} {
		sql := "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS " + n
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want accepted", sql, err)
		}
	}
	for _, n := range []string{"0", "10001", "100000", "-2"} {
		sql := "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS " + n
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) || !strings.Contains(err.Error(), "10000") {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want ErrDisallowedSQL naming 1..10000",
				sql, err)
		}
	}
	for _, sql := range []string{
		"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 100, DROP COLUMN x",
		"ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 100; DROP TABLE t",
	} {
		if err := ValidateExecutorSQL(sql); !errors.Is(err, ErrDisallowedSQL) {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want refused", sql, err)
		}
	}
}

// The SQL is built from identifiers quoted by sqlident: a quote or a
// semicolon in a name stays inside its quoted identifier. The validator's
// multi-statement guard refuses any ';' outside a literal, quoted
// identifiers included, so a name with a semicolon is quoted correctly
// and still refused by the executor (fail closed).
func TestColumnStatistics_BuilderQuotesIdentifiers(t *testing.T) {
	golden := []struct {
		schema, table, column string
		target                int
		want                  string
		runs                  bool
	}{
		{"public", "users", "deleted_at", 1000,
			"ALTER TABLE public.users ALTER COLUMN deleted_at SET STATISTICS 1000", true},
		{"public", "users", `we"ird col`, 100,
			`ALTER TABLE public.users ALTER COLUMN "we""ird col" SET STATISTICS 100`, true},
		{"public", "users", `we"ird;col`, 100,
			`ALTER TABLE public.users ALTER COLUMN "we""ird;col" SET STATISTICS 100`, false},
		{"App", "user", "Order; DROP TABLE x", -1,
			`ALTER TABLE "App"."user" ALTER COLUMN "Order; DROP TABLE x" SET STATISTICS -1`,
			false},
	}
	for _, g := range golden {
		got, err := SetColumnStatisticsSQL(g.schema, g.table, g.column, g.target)
		if err != nil || got != g.want {
			t.Fatalf("SetColumnStatisticsSQL(%q,%q,%q,%d) = %q, %v; want %q", g.schema,
				g.table, g.column, g.target, got, err, g.want)
		}
		if at := ActionTypeForSQL(got); at != ActionTypeSetColumnStatistics {
			t.Errorf("%q classifies as %q", got, at)
		}
		if err := ValidateExecutorSQL(got); (err == nil) != g.runs {
			t.Errorf("ValidateExecutorSQL(%q) = %v, want runs=%v", got, err, g.runs)
		}
		stmt, ok := ParseSetColumnStatistics(got)
		if !ok || stmt.Column != g.column || stmt.Target != g.target {
			t.Errorf("ParseSetColumnStatistics(%q) = %+v, %v; want column %q target %d", got,
				stmt, ok, g.column, g.target)
		}
	}
	for _, bad := range []int{0, -2, 10001} {
		if sql, err := SetColumnStatisticsSQL("public", "t", "c", bad); err == nil {
			t.Errorf("SetColumnStatisticsSQL target %d = %q, want refused", bad, sql)
		}
	}
	for _, empty := range [][3]string{{"", "t", "c"}, {"public", "", "c"}, {"public", "t", ""}} {
		if _, err := SetColumnStatisticsSQL(empty[0], empty[1], empty[2], 100); err == nil {
			t.Errorf("SetColumnStatisticsSQL(%q) accepted an empty name", empty)
		}
	}
}

func TestColumnStatistics_ParseNames(t *testing.T) {
	stmt, ok := ParseSetColumnStatistics(`ALTER TABLE "public"."Orders" ALTER "Mixed" ` +
		`SET STATISTICS 500`)
	want := ColumnStatisticsStmt{Table: `"public"."Orders"`, Column: "Mixed", Target: 500}
	if !ok || !reflect.DeepEqual(stmt, want) {
		t.Fatalf("parse quoted = %+v, %v; want %+v", stmt, ok, want)
	}
	stmt, ok = ParseSetColumnStatistics("ALTER TABLE Public.Orders ALTER COLUMN Status " +
		"SET STATISTICS 50")
	want = ColumnStatisticsStmt{Table: "Public.Orders", Column: "status", Target: 50}
	if !ok || !reflect.DeepEqual(stmt, want) {
		t.Fatalf("parse bare = %+v, %v; want %+v (bare names fold to lower case)", stmt, ok,
			want)
	}
	for _, sql := range []string{"", "ANALYZE public.t", "ALTER TABLE public.t SET (a=1)"} {
		if _, ok := ParseSetColumnStatistics(sql); ok {
			t.Errorf("ParseSetColumnStatistics(%q) matched", sql)
		}
	}
}

// The rollback restores the prior target: -1 for the default (-1, or
// NULL on PostgreSQL 17+); a prior the contract would refuse (0) has no
// faithful rollback, so the change is refused.
func TestColumnStatistics_RollbackFromPrior(t *testing.T) {
	stmt := ColumnStatisticsStmt{Table: "public.t", Column: "c", Target: 1000}
	n := func(v int16) *int16 { return &v }
	cases := []struct {
		prior *int16
		want  string
	}{
		{nil, "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS -1"},
		{n(-1), "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS -1"},
		{n(250), "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 250"},
		{n(10000), "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 10000"},
	}
	for _, tc := range cases {
		got, err := columnStatisticsRollbackSQL(stmt, tc.prior)
		if err != nil || got != tc.want {
			t.Errorf("rollback for prior %v = %q, %v; want %q", tc.prior, got, err, tc.want)
		}
		if at := ActionTypeForSQL(got); at != ActionTypeSetColumnStatistics {
			t.Errorf("rollback %q classifies as %q", got, at)
		}
	}
	if got, err := columnStatisticsRollbackSQL(stmt, n(0)); !errors.Is(err,
		ErrNoFaithfulRollback) {
		t.Fatalf("rollback for prior 0 = %q, %v; want ErrNoFaithfulRollback", got, err)
	}
	quoted := ColumnStatisticsStmt{Table: `"public"."T"`, Column: `a"b`, Target: 100}
	got, err := columnStatisticsRollbackSQL(quoted, nil)
	if err != nil || got != `ALTER TABLE "public"."T" ALTER COLUMN "a""b" SET STATISTICS -1` {
		t.Fatalf("quoted rollback = %q, %v", got, err)
	}
}

func TestColumnStatistics_ReadbackMatches(t *testing.T) {
	n := func(v int16) *int16 { return &v }
	cases := []struct {
		want  int
		live  *int16
		match bool
	}{
		{1000, n(1000), true}, {1000, n(100), false}, {1000, nil, false},
		{-1, nil, true}, {-1, n(-1), true}, {-1, n(100), false},
	}
	for _, tc := range cases {
		if got := statTargetMatches(tc.want, tc.live); got != tc.match {
			t.Errorf("statTargetMatches(%d, %v) = %v, want %v", tc.want, tc.live, got, tc.match)
		}
	}
}

func TestColumnStatistics_ContractShape(t *testing.T) {
	c, ok := ContractForActionType(ActionTypeSetColumnStatistics)
	if !ok {
		t.Fatal("set_column_statistics has no contract")
	}
	if c.BaseRiskTier != "moderate" || c.RollbackClass != "reversible" {
		t.Fatalf("contract risk/rollback = %s/%s, want moderate/reversible", c.BaseRiskTier,
			c.RollbackClass)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("contract does not validate: %v", err)
	}
	joined := strings.Join(c.Guardrails, " | ")
	for _, want := range []string{"lock_timeout", "SHARE UPDATE EXCLUSIVE", "1..10000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("guardrails %q do not name %q", joined, want)
		}
	}
	if !strings.Contains(strings.Join(c.PostChecks, " "), "attstattarget") {
		t.Errorf("post-checks %q do not read back attstattarget", c.PostChecks)
	}
	// Must not change: the alter_table contract stays high and forward-fix.
	alter, _ := ContractForActionType("alter_table")
	if alter.BaseRiskTier != "high" || alter.RollbackClass != "forward_fix_only" {
		t.Fatalf("alter_table changed to %s/%s", alter.BaseRiskTier, alter.RollbackClass)
	}
}

func TestColumnStatistics_PolicyLadderAndLease(t *testing.T) {
	if got := changeClassForActionType(ActionTypeSetColumnStatistics); got !=
		string(policy.ChangeAnalyze) {
		t.Fatalf("change class = %q, want %q (planner statistics)", got, policy.ChangeAnalyze)
	}
	sql := "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 1000"
	if cheap, why := RevertCost(ActionTypeSetColumnStatistics, sql); !cheap {
		t.Fatalf("RevertCost = costly (%s), want cheap: SET STATISTICS -1 undoes it", why)
	}
	if got := operatorLeaseTargets(sql); !reflect.DeepEqual(got, []string{"public.t"}) {
		t.Fatalf("lease targets = %q, want [public.t]", got)
	}
	if !isLeasedMutation(sql) {
		t.Fatal("SET STATISTICS must take the table's change lease")
	}
	rb := "ALTER TABLE public.t ALTER COLUMN c SET STATISTICS -1"
	contract, ok := rollbackContractFor(sql, rb)
	if !ok || contract.ActionType != ActionTypeSetColumnStatistics {
		t.Fatalf("rollback contract = %q, %v; want set_column_statistics", contract.ActionType,
			ok)
	}
	if at, tier := ClassifySQL(sql); at != ActionTypeSetColumnStatistics || tier != "moderate" {
		t.Fatalf("ClassifySQL = %s/%s, want set_column_statistics/moderate", at, tier)
	}
}

// ALTER TABLE ONLY reaches the named table alone, so its rollback must
// too: a rollback without ONLY would reset every inheritance child and
// partition the forward never touched (review-02c review).
func TestColumnStatistics_OnlyKeptInRollback(t *testing.T) {
	stmt, ok := ParseSetColumnStatistics("ALTER TABLE ONLY public.t ALTER COLUMN c " +
		"SET STATISTICS 500")
	want := ColumnStatisticsStmt{Table: "public.t", Column: "c", Target: 500, Only: true}
	if !ok || !reflect.DeepEqual(stmt, want) {
		t.Fatalf("parse ONLY = %+v, %v; want %+v", stmt, ok, want)
	}
	prior := int16(40)
	got, err := columnStatisticsRollbackSQL(stmt, &prior)
	if err != nil || got != "ALTER TABLE ONLY public.t ALTER COLUMN c SET STATISTICS 40" {
		t.Fatalf("ONLY rollback = %q, %v; want ONLY kept", got, err)
	}
	if at := ActionTypeForSQL(got); at != ActionTypeSetColumnStatistics {
		t.Fatalf("ONLY rollback %q classifies as %q", got, at)
	}
	plain, ok := ParseSetColumnStatistics("ALTER TABLE public.t ALTER COLUMN c " +
		"SET STATISTICS 500")
	if !ok || plain.Only {
		t.Fatalf("parse without ONLY = %+v, %v; want Only false", plain, ok)
	}
}
