package impact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Pure parts of the engine: lock conflicts, the queue formula, severity
// bars, type names, evidence references, post-conditions and landing.
// No concurrent-access tests here: these functions are stateless.

func TestNewRefusesMissingPool(t *testing.T) {
	_, err := New(Deps{})
	if !errors.Is(err, changereview.ErrInvalid) {
		t.Fatalf("New without a pool: got %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "pool") {
		t.Fatalf("error %q does not name the pool", err)
	}
}

func TestConflictMatrix(t *testing.T) {
	cases := []struct {
		ddl  string
		work sqlast.LockMode
		want bool
	}{
		{lockAccessExclusive, sqlast.LockAccessShare, true},
		{lockAccessExclusive, sqlast.LockRowShare, true},
		{lockAccessExclusive, sqlast.LockRowExclusive, true},
		{lockExclusive, sqlast.LockAccessShare, false},
		{lockExclusive, sqlast.LockRowShare, true},
		{lockExclusive, sqlast.LockRowExclusive, true},
		{lockShareRowExclusive, sqlast.LockAccessShare, false},
		{lockShareRowExclusive, sqlast.LockRowExclusive, true},
		{lockShare, sqlast.LockAccessShare, false},
		{lockShare, sqlast.LockRowShare, false},
		{lockShare, sqlast.LockRowExclusive, true},
		{lockShareUpdateExclusive, sqlast.LockRowExclusive, false},
		{lockShareUpdateExclusive, sqlast.LockAccessShare, false},
		{"", sqlast.LockRowExclusive, false},
		{lockAccessExclusive, sqlast.LockNone, false},
	}
	for _, c := range cases {
		if got := conflicts(c.ddl, c.work); got != c.want {
			t.Errorf("conflicts(%q, %q) = %v, want %v", c.ddl, c.work, got, c.want)
		}
	}
}

func TestStatementLockByKind(t *testing.T) {
	cases := []struct {
		name string
		p    sqlast.DDLStatement
		want string
	}{
		{"create index", sqlast.DDLStatement{Kind: sqlast.DDLCreateIndex,
			Index: &sqlast.IndexDef{}}, lockShare},
		{"create index concurrently", sqlast.DDLStatement{Kind: sqlast.DDLCreateIndex,
			Index: &sqlast.IndexDef{Concurrent: true}}, lockShareUpdateExclusive},
		{"drop index", sqlast.DDLStatement{Kind: sqlast.DDLDropIndex,
			Index: &sqlast.IndexDef{}}, lockAccessExclusive},
		{"drop index concurrently", sqlast.DDLStatement{Kind: sqlast.DDLDropIndex,
			Index: &sqlast.IndexDef{Concurrent: true}}, lockShareUpdateExclusive},
		{"add column", sqlast.DDLStatement{Kind: sqlast.DDLAddColumn}, lockAccessExclusive},
		{"alter type", sqlast.DDLStatement{Kind: sqlast.DDLAlterType}, lockAccessExclusive},
		{"add fk", sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
			Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintForeignKey}},
			lockShareRowExclusive},
		{"add check", sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint,
			Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintCheck}},
			lockAccessExclusive},
		{"validate", sqlast.DDLStatement{Kind: sqlast.DDLValidateConstraint},
			lockShareUpdateExclusive},
		{"statistics", sqlast.DDLStatement{Kind: sqlast.DDLCreateStatistics},
			lockShareUpdateExclusive},
		{"create table", sqlast.DDLStatement{Kind: sqlast.DDLCreateTable}, ""},
		{"update", sqlast.DDLStatement{Kind: sqlast.DDLUpdate}, ""},
		{"set", sqlast.DDLStatement{Kind: sqlast.DDLSet}, ""},
	}
	for _, c := range cases {
		if got := statementLock(c.p); got != c.want {
			t.Errorf("%s: statementLock = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestQueueFormula(t *testing.T) {
	if got := queueEstimate(8.5, 2000); got != 17 {
		t.Fatalf("queueEstimate(8.5/s, 2000ms) = %v, want 17", got)
	}
	if got := queueEstimate(0, 5000); got != 0 {
		t.Fatalf("queueEstimate with no traffic = %v, want 0", got)
	}
	if got := queueEstimate(-3, 1000); got != 0 {
		t.Fatalf("queueEstimate with a negative rate = %v, want 0", got)
	}
	if got := waitingExposure(10, nil); got != nil {
		t.Fatalf("waitingExposure without lock_timeout = %v, want unbounded (nil)", *got)
	}
	zero := int64(0)
	if got := waitingExposure(10, &zero); got != nil {
		t.Fatalf("lock_timeout 0 (disabled) = %v, want unbounded (nil)", *got)
	}
	two := int64(2000)
	got := waitingExposure(10, &two)
	if got == nil || *got != 20 {
		t.Fatalf("waitingExposure(10/s, 2s) = %v, want 20", got)
	}
	if hold := indexBuildHoldMs(50 << 20); hold != 1000 {
		t.Fatalf("index build over 50 MiB = %d ms, want 1000", hold)
	}
	if hold := indexBuildHoldMs(0); hold != 0 {
		t.Fatalf("index build over an unknown heap = %d ms, want 0", hold)
	}
}

func TestQueueSeverityBoundaries(t *testing.T) {
	cfg := config.DefaultChangeReviewConfig()
	cases := []struct {
		queued float64
		want   string
	}{
		{0, changereview.SeverityInfo},
		{99.99, changereview.SeverityInfo},
		{100, changereview.SeverityWarning},
		{4999, changereview.SeverityWarning},
		{5000, changereview.SeverityCritical},
		{1e9, changereview.SeverityCritical},
	}
	for _, c := range cases {
		if got := queueSeverity(c.queued, cfg); got != c.want {
			t.Errorf("queueSeverity(%v) = %s, want %s", c.queued, got, c.want)
		}
	}
}

func TestBackfillSeverityBoundaries(t *testing.T) {
	cfg := config.DefaultChangeReviewConfig()
	cases := []struct {
		rows float64
		want string
	}{
		{0, changereview.SeverityInfo},
		{999_999, changereview.SeverityInfo},
		{1_000_000, changereview.SeverityWarning},
		{9_999_999, changereview.SeverityWarning},
		{10_000_000, changereview.SeverityCritical},
	}
	for _, c := range cases {
		if got := backfillSeverity(c.rows, cfg); got != c.want {
			t.Errorf("backfillSeverity(%v) = %s, want %s", c.rows, got, c.want)
		}
	}
}

func TestFormatTypeMatchesCatalogNames(t *testing.T) {
	cases := map[string]string{
		"int4":          "integer",
		"int8":          "bigint",
		"int2":          "smallint",
		"bool":          "boolean",
		"varchar(20)":   "character varying(20)",
		"varchar":       "character varying",
		"bpchar(3)":     "character(3)",
		"timestamptz":   "timestamp with time zone",
		"timestamp":     "timestamp without time zone",
		"timetz":        "time with time zone",
		"float8":        "double precision",
		"float4":        "real",
		"numeric(12,2)": "numeric(12,2)",
		"text":          "text",
		"uuid":          "uuid",
		"int4[]":        "integer[]",
		"":              "",
	}
	for in, want := range cases {
		if got := formatType(in); got != want {
			t.Errorf("formatType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEvidenceRefIsAlwaysValid(t *testing.T) {
	for _, ref := range []string{"rv.rv_orders", `App."Orders Table"`, "naïve", "",
		strings.Repeat("x", 400), "a b\tc"} {
		got := evidenceRef(ref)
		if !changereview.ValidEvidenceID("table:" + got) {
			t.Errorf("evidenceRef(%q) = %q is not a valid evidence reference", ref, got)
		}
	}
	if evidenceRef("rv.rv_orders") != "rv.rv_orders" {
		t.Fatalf("a plain name must be kept as it is, got %q", evidenceRef("rv.rv_orders"))
	}
	if evidenceRef("a b") == evidenceRef("a_b") {
		t.Fatal("names that differ only by a replaced character must not collide")
	}
}

func TestPostConditionPerDDLKind(t *testing.T) {
	rel := sqlast.QualifiedName{Schema: "rv", Name: "rv_orders"}
	cases := []struct {
		p    sqlast.DDLStatement
		want changereview.PostCondition
	}{
		{sqlast.DDLStatement{Kind: sqlast.DDLCreateIndex, Relation: rel,
			Object: sqlast.QualifiedName{Schema: "rv", Name: "ix"},
			Index:  &sqlast.IndexDef{Name: "ix"}},
			changereview.PostCondition{Kind: changereview.PostIndexValid,
				Relation: "rv.rv_orders", Index: "ix"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLAddColumn, Relation: rel,
			Column: &sqlast.ColumnDef{Name: "note", Type: "text"}},
			changereview.PostCondition{Kind: changereview.PostColumnPresent,
				Relation: "rv.rv_orders", Column: "note", Type: "text"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLDropColumn, Relation: rel,
			Column: &sqlast.ColumnDef{Name: "status"}},
			changereview.PostCondition{Kind: changereview.PostColumnAbsent,
				Relation: "rv.rv_orders", Column: "status"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLRenameColumn, Relation: rel,
			Columns: []string{"email"}, NewName: "email_address"},
			changereview.PostCondition{Kind: changereview.PostColumnRenamed,
				Relation: "rv.rv_orders", Column: "email", NewName: "email_address"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLAlterType, Relation: rel,
			Column: &sqlast.ColumnDef{Name: "code", Type: "varchar(40)"}},
			changereview.PostCondition{Kind: changereview.PostColumnType,
				Relation: "rv.rv_orders", Column: "code", Type: "character varying(40)"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLSetNotNull, Relation: rel,
			Column: &sqlast.ColumnDef{Name: "customer_id"}},
			changereview.PostCondition{Kind: changereview.PostNotNull,
				Relation: "rv.rv_orders", Column: "customer_id"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLDropNotNull, Relation: rel,
			Column: &sqlast.ColumnDef{Name: "email"}},
			changereview.PostCondition{Kind: changereview.PostNullable,
				Relation: "rv.rv_orders", Column: "email"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLRenameTable, Relation: rel,
			NewName: "orders2"},
			changereview.PostCondition{Kind: changereview.PostRelationRenamed,
				Relation: "rv.rv_orders", NewName: "orders2"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLDropTable, Relation: rel},
			changereview.PostCondition{Kind: changereview.PostRelationAbsent,
				Relation: "rv.rv_orders"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLCreateTable, Relation: rel},
			changereview.PostCondition{Kind: changereview.PostRelationPresent,
				Relation: "rv.rv_orders"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLDropConstraint, Relation: rel,
			Constraint: &sqlast.ConstraintDef{Name: "c1"}},
			changereview.PostCondition{Kind: changereview.PostConstraintAbsent,
				Relation: "rv.rv_orders", Constraint: "c1"}},
		{sqlast.DDLStatement{Kind: sqlast.DDLValidateConstraint, Relation: rel,
			Constraint: &sqlast.ConstraintDef{Name: "c1"}},
			changereview.PostCondition{Kind: changereview.PostConstraint,
				Relation: "rv.rv_orders", Constraint: "c1", Type: constraintValidated}},
	}
	for _, c := range cases {
		got, ok := postCondition(3, c.p, "")
		c.want.StatementIndex = 3
		if !ok || got != c.want {
			t.Errorf("%s: postCondition = %+v (%v), want %+v", c.p.Kind, got, ok, c.want)
		}
	}
}

func TestPostConditionNamesWhatPostgresGenerates(t *testing.T) {
	rel := sqlast.QualifiedName{Schema: "rv", Name: "rv_items"}
	fk := sqlast.DDLStatement{Kind: sqlast.DDLAddConstraint, Relation: rel,
		Constraint: &sqlast.ConstraintDef{Kind: sqlast.ConstraintForeignKey,
			Columns: []string{"order_id"}}}
	got, ok := postCondition(1, fk, "")
	if !ok || got.Constraint != "rv_items_order_id_fkey" {
		t.Fatalf("unnamed FK: %+v (%v), want constraint rv_items_order_id_fkey", got, ok)
	}
	ix := sqlast.DDLStatement{Kind: sqlast.DDLCreateIndex, Relation: rel,
		Index: &sqlast.IndexDef{Columns: []string{"order_id", "qty"}}}
	got, ok = postCondition(1, ix, "")
	if !ok || got.Index != "rv_items_order_id_qty_idx" {
		t.Fatalf("unnamed index: %+v (%v), want rv_items_order_id_qty_idx", got, ok)
	}
	drop := sqlast.DDLStatement{Kind: sqlast.DDLDropIndex,
		Object: sqlast.QualifiedName{Schema: "rv", Name: "rv_orders_created_idx"},
		Index:  &sqlast.IndexDef{Name: "rv_orders_created_idx"}}
	got, ok = postCondition(2, drop, "rv.rv_orders")
	if !ok || got.Kind != changereview.PostIndexAbsent || got.Relation != "rv.rv_orders" ||
		got.Index != "rv_orders_created_idx" {
		t.Fatalf("drop index: %+v (%v)", got, ok)
	}
	for _, k := range []sqlast.DDLKind{sqlast.DDLSet, sqlast.DDLTransaction,
		sqlast.DDLUpdate, sqlast.DDLInsert, sqlast.DDLDelete, sqlast.DDLOther} {
		if pc, ok := postCondition(1, sqlast.DDLStatement{Kind: k}, ""); ok {
			t.Errorf("%s is not DDL but got post-condition %+v", k, pc)
		}
	}
}

func TestLandingAggregation(t *testing.T) {
	r := func(states ...string) []PostResult {
		out := make([]PostResult, len(states))
		for i, s := range states {
			out[i] = PostResult{State: s}
		}
		return out
	}
	cases := []struct {
		in   []PostResult
		want string
	}{
		{nil, ""},
		{r(PostLanded), PostLanded},
		{r(PostLanded, PostLanded), PostLanded},
		{r(PostLanded, PostNotLanded), PostNotLanded},
		{r(PostNotLanded, PostInvalid), PostInvalid},
		{r(PostUnknown, PostLanded), PostNotLanded},
	}
	for i, c := range cases {
		if got := Landing(c.in); got != c.want {
			t.Errorf("case %d: Landing = %q, want %q", i, got, c.want)
		}
	}
}

func TestEvaluateEmptyPostConditionsReadsNothing(t *testing.T) {
	got, err := EvaluatePostConditions(context.Background(), nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("no post-conditions: got %v, %v; want none and no error", got, err)
	}
	landed, err := AlreadyLanded(context.Background(), nil, changereview.Expectations{})
	if err != nil || landed {
		t.Fatalf("no post-conditions are never already landed: got %v, %v", landed, err)
	}
}

func TestEvaluateRefusesUnknownKind(t *testing.T) {
	_, err := EvaluatePostConditions(context.Background(), nil,
		[]changereview.PostCondition{{Kind: "index_exploded", Relation: "rv.t"}})
	if !errors.Is(err, changereview.ErrInvalid) || !strings.Contains(err.Error(),
		"index_exploded") {
		t.Fatalf("unknown kind: got %v, want ErrInvalid naming the kind", err)
	}
	_, err = EvaluatePostConditions(context.Background(), nil,
		[]changereview.PostCondition{{Kind: changereview.PostRelationPresent,
			Relation: "no_schema"}})
	if !errors.Is(err, changereview.ErrInvalid) {
		t.Fatalf("relation without a schema: got %v, want ErrInvalid", err)
	}
}
