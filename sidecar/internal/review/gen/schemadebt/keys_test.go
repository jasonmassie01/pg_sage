package schemadebt

import (
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// No concurrent access tests: Build is a pure function over the pack and
// the inputs; state transitions of proposals belong to plan-01.

const capacityAction = "prepare_sequence_capacity_migration"

func TestBuild_Int4KeyUnder365DaysIsACapacityProposalCitingTheSamples(t *testing.T) {
	pack := schemaPack(t, nil, nil, ordersTable(1<<20))
	cands := build(t, pack, keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max)), Options{})
	c := only(t, cands, KindInt4Key)
	if c.ActionType != capacityAction || c.Approval != candidate.ApprovalRequired {
		t.Fatalf("key candidate %s / %s, want %s needing approval", c.ActionType, c.Approval,
			capacityAction)
	}
	if c.Target != "public.orders.id" || !slices.Contains(c.EvidenceIDs,
		"schema/table/public.orders") {
		t.Fatalf("target %q evidence %v", c.Target, c.EvidenceIDs)
	}
	mustContain(t, "reason", c.Reason, "exhausts in 247 days", "public.orders.id", "integer")
	mustContain(t, "source", c.Prediction.Source, "sage.runway_samples", ordersSeq,
		"epoch e1", "5 samples")
	if q := c.Prediction.Quantities; q[QuantityRunwayDays] != 247 ||
		q[QuantityMeasured] != 1 || q[QuantityInt4Key] != 1 || q[QuantitySamples] != 5 {
		t.Fatalf("quantities %v", q)
	}
	mustContain(t, "forward SQL", c.ForwardSQL, `"public"."orders_id_seq"`,
		"pg_sage never runs this script", "Measured rate")
	swap := only(t, cands, KindColumnSwap)
	if swap.ActionType != candidate.ActionAdvice ||
		swap.Approval != candidate.ApprovalRequired {
		t.Fatalf("swap %s / %s, want advice needing approval", swap.ActionType,
			swap.Approval)
	}
	mustContain(t, "swap SQL", swap.ForwardSQL,
		"Online column swap of public.orders.id: integer -> bigint")
}

func TestBuild_DescendingKeyRunsTowardTheMinimum(t *testing.T) {
	in := keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max+1))
	col := in.Columns[ordersID]
	col.Increment = -1
	in.Columns[ordersID] = col
	c := only(t, build(t, schemaPack(t, nil, nil, ordersTable(1<<20)), in, Options{}),
		KindInt4Key)
	mustContain(t, "reason", c.Reason, "exhausts in 247 days", "minimum -2147483648")
}

func TestBuild_BigintKeyGivesNoDebtItem(t *testing.T) {
	tab := ordersTable(1 << 20)
	tab.Columns[0].Type = "bigint"
	in := keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, 9.2e18))
	if cands := build(t, schemaPack(t, nil, nil, tab), in, Options{}); len(cands) != 0 {
		t.Fatalf("bigint key gave %+v", cands)
	}
}

func TestBuild_NoSamplesSaysRateNotMeasuredAndNoDate(t *testing.T) {
	in := keyInputs(Series{Subject: ordersSeq})
	in.Series = nil
	c := only(t, build(t, schemaPack(t, nil, nil, ordersTable(1<<20)), in, Options{}),
		KindInt4Key)
	if c.ActionType != candidate.ActionAdvice {
		t.Fatalf("unmeasured key is %s, want advice", c.ActionType)
	}
	mustContain(t, "reason", c.Reason, RateNotMeasured)
	if strings.Contains(c.Reason, "exhausts") {
		t.Fatalf("reason %q guesses a date", c.Reason)
	}
	if _, ok := c.Prediction.Quantities[QuantityRunwayDays]; ok ||
		c.Prediction.Quantities[QuantityMeasured] != 0 {
		t.Fatalf("quantities %v state a runway", c.Prediction.Quantities)
	}
}

func TestBuild_WithoutCatalogFactsTheKeyIsStillNamed(t *testing.T) {
	pack := schemaPack(t, nil, nil, ordersTable(1<<20))
	cands, cov, err := Build(t.Context(), pack, Inputs{}, env(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := only(t, cands, KindInt4Key)
	mustContain(t, "reason", c.Reason, RateNotMeasured)
	if !slices.Contains(cov.Missing, "catalog") || cov.Status != candidate.CoveragePartial {
		t.Fatalf("coverage %+v, want catalog missing", cov)
	}
}

func TestBuild_KeyWithoutASequenceIsLeftOut(t *testing.T) {
	in := keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max))
	col := in.Columns[ordersID]
	col.Sequence, col.Facts.Column.SequenceName, col.Facts.Column.Default = "", "", ""
	in.Columns[ordersID] = col
	pack := schemaPack(t, nil, nil, ordersTable(1<<20))
	cands, cov, err := Build(t.Context(), pack, in, env(), Options{})
	if err != nil || len(ofKind(cands, KindInt4Key)) != 0 {
		t.Fatalf("natural int4 key gave %+v (%v)", cands, err)
	}
	if len(cov.Notes) == 0 {
		t.Fatalf("coverage %+v does not say why the key was left out", cov)
	}
}

func TestBuild_NoTablesGivesNoOutput(t *testing.T) {
	cands, _, err := Build(t.Context(), schemaPack(t, nil, nil), Inputs{}, env(), Options{})
	if err != nil || len(cands) != 0 {
		t.Fatalf("no tables gave %+v (%v)", cands, err)
	}
}

// Error propagation: the planner refuses a default it cannot read as the
// column's own sequence; the refusal is attached and the runway kept.
func TestBuild_PlannerRefusalAttachesTheReasonAndKeepsTheRunway(t *testing.T) {
	in := keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max))
	col := in.Columns[ordersID]
	col.Facts.Column.Default = "nextval('public.orders_id_seq'::regclass) * 2"
	in.Columns[ordersID] = col
	cands := build(t, schemaPack(t, nil, nil, ordersTable(1<<20)), in, Options{})
	c := only(t, cands, KindInt4Key)
	if c.ActionType != capacityAction {
		t.Fatalf("refused swap changed the key to %s", c.ActionType)
	}
	mustContain(t, "reason", c.Reason, "exhausts in 247 days", "online column swap refused",
		"plan it by hand")
	if n := len(ofKind(cands, KindColumnSwap)); n != 0 {
		t.Fatalf("%d swap candidates after a refusal", n)
	}
}

func TestBuild_CatalogReadErrorIsAttached(t *testing.T) {
	in := Inputs{Columns: map[ColumnRef]Column{ordersID: {ReadErr: "permission denied"}}}
	c := only(t, build(t, schemaPack(t, nil, nil, ordersTable(1<<20)), in, Options{}),
		KindInt4Key)
	mustContain(t, "reason", c.Reason, "permission denied", RateNotMeasured)
}

func TestBuild_RunwayBoundary365IsAdviceAnd364IsAProposal(t *testing.T) {
	for days, want := range map[float64]string{365: candidate.ActionAdvice,
		364: capacityAction, 366: candidate.ActionAdvice} {
		s := linear(ordersSeq, int4Max-days*1e6, 1e6, 3, int4Max)
		c := only(t, build(t, schemaPack(t, nil, nil, ordersTable(1<<20)), keyInputs(s),
			Options{}), KindInt4Key)
		if c.ActionType != want || c.Prediction.Quantities[QuantityRunwayDays] != days {
			t.Errorf("%v days: %s with %v days, want %s", days, c.ActionType,
				c.Prediction.Quantities[QuantityRunwayDays], want)
		}
	}
}

// accountsAndOrders is public.accounts (bigint key) and public.orders,
// whose integer account_id references it.
func accountsAndOrders() []workpack.TableFact {
	acc := workpack.TableFact{ID: "schema/table/public.accounts", Table: "public.accounts",
		Kind: "table", Columns: []workpack.ColumnFact{{Name: "id", Type: "bigint"}},
		Constraints: []string{"accounts_pkey PRIMARY KEY (id)"}}
	ord := ordersTable(1 << 20)
	ord.Columns[0].Type = "bigint"
	ord.Columns = append(ord.Columns, workpack.ColumnFact{Name: "account_id", Type: "integer"})
	ord.Constraints = append(ord.Constraints,
		"orders_account_fk FOREIGN KEY (account_id) REFERENCES public.accounts(id)")
	return []workpack.TableFact{acc, ord}
}

func fkInputs() Inputs {
	accID := ColumnRef{Schema: "public", Table: "accounts", Column: "id"}
	ordAcc := ColumnRef{Schema: "public", Table: "orders", Column: "account_id"}
	accFacts := serialFacts("public", "accounts", "id")
	accFacts.Column.Type, accFacts.Column.SequenceType = "bigint", "bigint"
	child := serialFacts("public", "orders", "id")
	child.PrimaryKey.ColumnType = "bigint"
	child.Column = plan.SwapColumn{Name: "account_id", Type: "integer"}
	seq := "public.accounts_id_seq"
	return Inputs{Columns: map[ColumnRef]Column{
		accID:  {Facts: accFacts, Sequence: seq, Increment: 1},
		ordAcc: {Facts: child}},
		Series: map[string]Series{seq: linear(seq, 1.9e9, 1e6, 5, 9.2e18)}}
}

func TestBuild_FKTypeMismatchUsesTheParentRateAndTheChildLimit(t *testing.T) {
	cands := build(t, schemaPack(t, nil, nil, accountsAndOrders()...), fkInputs(), Options{})
	c := only(t, cands, KindFKMismatch)
	if c.ActionType != candidate.ActionAdvice || c.Target != "public.orders.account_id" {
		t.Fatalf("fk candidate %s on %s", c.ActionType, c.Target)
	}
	mustContain(t, "reason", c.Reason, "public.orders.account_id", "integer",
		"public.accounts.id", "bigint", "exhausts in 247 days")
	for _, id := range []string{"schema/table/public.orders", "schema/table/public.accounts"} {
		if !slices.Contains(c.EvidenceIDs, id) {
			t.Errorf("evidence %v lacks %s", c.EvidenceIDs, id)
		}
	}
	swap := only(t, cands, KindColumnSwap)
	mustContain(t, "swap SQL", swap.ForwardSQL, "public.orders.account_id: integer -> bigint")
	if n := len(ofKind(cands, KindInt4Key)); n != 0 {
		t.Fatalf("bigint keys gave %d int4 key items", n)
	}
}

func TestBuild_FKToANarrowerParentIsNoMismatch(t *testing.T) {
	tabs := accountsAndOrders()
	tabs[0].Columns[0].Type = "integer"
	tabs[1].Columns[3].Type = "bigint"
	cands := build(t, schemaPack(t, nil, nil, tabs...), Inputs{}, Options{})
	if n := len(ofKind(cands, KindFKMismatch)); n != 0 {
		t.Fatalf("a wider child gave %d mismatch items: %+v", n, cands)
	}
}
