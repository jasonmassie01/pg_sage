package indexes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

func TestGenerate_ChainGivesOneConsolidationSetWithTwoDrops(t *testing.T) {
	m := measured()
	pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
	cands, cov, err := New(m).Generate(context.Background(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || cov.Status != candidate.CoverageComplete {
		t.Fatalf("%d candidates (%+v), coverage %+v; want 2, complete", len(cands), cands, cov)
	}
	got := byTarget(cands)
	for _, name := range []string{"t_a", "t_ab"} {
		c, ok := got["public."+name]
		if !ok {
			t.Fatalf("no step drops public.%s: %+v", name, cands)
		}
		if c.ActionType != "drop_unused_index" || c.Kind != KindConsolidation ||
			c.Generator != Name || c.Dimension != Dimension {
			t.Errorf("%s: %s/%s/%s/%s", name, c.ActionType, c.Kind, c.Generator, c.Dimension)
		}
		if c.ForwardSQL != `DROP INDEX CONCURRENTLY "public"."`+name+`";` {
			t.Errorf("%s forward SQL %q", name, c.ForwardSQL)
		}
		if !strings.HasPrefix(c.RollbackSQL, "CREATE INDEX "+name+" ON public.t") ||
			!strings.HasSuffix(c.RollbackSQL, ";") || !c.Reversible {
			t.Errorf("%s rollback %q reversible %v", name, c.RollbackSQL, c.Reversible)
		}
		want := []string{"index/idx/public." + name, "index/idx/public.t_abc"}
		if !slices.Equal(c.EvidenceIDs, want) {
			t.Errorf("%s evidence %v, want %v", name, c.EvidenceIDs, want)
		}
		if c.Prediction.Tier != "plan_only" || !strings.Contains(c.Reason, "t_abc") {
			t.Errorf("%s tier %q reason %q", name, c.Prediction.Tier, c.Reason)
		}
		if err := c.Validate(pack); err != nil {
			t.Errorf("%s does not validate: %v", name, err)
		}
	}
	if _, ok := got["public.t_abc"]; ok {
		t.Error("the covering index is dropped")
	}
	if len(m.sets) != 1 || !slices.Equal(m.sets[0].Indexes, []string{"t_a", "t_ab"}) ||
		m.sets[0].Schema != "public" || m.sets[0].MaxRegressionPct != 10 {
		t.Fatalf("measured sets %+v, want one set of t_a, t_ab at 10%%", m.sets)
	}
	if q := m.sets[0].Queries; len(q) != 2 || q[0].QueryID != 9 || q[1].QueryID != 7 ||
		q[0].TotalTimeMs != 900 || q[0].Text == "" {
		t.Fatalf("the set was planned over %+v, want queryids 9 then 7 with texts", q)
	}
}

func TestGenerate_WriteCostAndBytesAreStatedPerStep(t *testing.T) {
	pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
	cands, _, err := New(measured()).Generate(context.Background(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	c := byTarget(cands)["public.t_ab"]
	q := c.Prediction.Quantities
	// (900 inserts + 900 updates) / 90 days, one leaf block per index each.
	if q[QuantityBytes] != 2000 || q[QuantityWriteBlocks] != 20 {
		t.Fatalf("quantities %v, want 2000 bytes and 20 write blocks a day", q)
	}
	if c.Prediction.SizePct == nil || *c.Prediction.SizePct < 33.3 ||
		*c.Prediction.SizePct > 33.4 || c.Prediction.Direction != "decrease" ||
		c.Prediction.Metric != MetricWriteBlocks {
		t.Fatalf("prediction %+v, want a third of the table's index writes saved",
			c.Prediction)
	}
	bytes, blocks := Totals(cands)
	if bytes != 3000 || blocks != 40 {
		t.Fatalf("totals %d bytes, %v blocks; want 3000 and 40", bytes, blocks)
	}
}

func TestGenerate_UnknownWritesLeaveTheWriteCostOut(t *testing.T) {
	facts := chainFacts()
	for i := range facts {
		facts[i].TableWrites = "unknown"
	}
	pack := mustPack(t, spec("db1", 90, "ok", facts...))
	cands, _, err := New(measured()).Generate(context.Background(), pack, env("db1"))
	if err != nil || len(cands) != 2 {
		t.Fatalf("%d candidates, err %v", len(cands), err)
	}
	for _, c := range cands {
		if _, ok := c.Prediction.Quantities[QuantityWriteBlocks]; ok {
			t.Errorf("%s states a write cost from unknown writes", c.Target)
		}
		if c.Prediction.Metric != MetricIndexBytes || c.Prediction.SizePct != nil {
			t.Errorf("%s prediction %+v, want %q with no size", c.Target, c.Prediction,
				MetricIndexBytes)
		}
	}
}

func TestGenerate_WithoutHypoPGTheSetIsAdviceAndDuplicatesStaySteps(t *testing.T) {
	facts := append(chainFacts(), fact("u", "u_x1", "x", 500), fact("u", "u_x2", "x", 600))
	facts[3].DuplicateOf = []string{"public.u_x2"}
	facts[4].DuplicateOf = []string{"public.u_x1"}
	pack := mustPack(t, spec("db1", 90, "ok", facts...))
	cands, _, err := New(nil).Generate(context.Background(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	got := byTarget(cands)
	for _, name := range []string{"public.t_a", "public.t_ab"} {
		c := got[name]
		if c.ActionType != candidate.ActionAdvice || c.Prediction.Tier != candidate.TierNone ||
			!strings.Contains(c.Reason, "hypopg") || c.Prediction.Direction != "unknown" {
			t.Errorf("%s: %s tier %q direction %q reason %q", name, c.ActionType,
				c.Prediction.Tier, c.Prediction.Direction, c.Reason)
		}
	}
	dup, ok := got["public.u_x2"]
	if !ok || dup.ActionType != "drop_unused_index" || dup.Kind != KindDuplicate {
		t.Fatalf("duplicate u_x2: %+v (found %v)", dup, ok)
	}
	if _, ok := got["public.u_x1"]; ok {
		t.Error("both copies of the duplicate are dropped")
	}
	if !slices.Equal(dup.EvidenceIDs, []string{"index/idx/public.u_x2",
		"index/idx/public.u_x1"}) {
		t.Errorf("duplicate evidence %v", dup.EvidenceIDs)
	}
	if err := dup.Validate(pack); err != nil {
		t.Errorf("duplicate does not validate: %v", err)
	}
}

func TestGenerate_HypoPGErrorDowngradesTheSetToAdviceWithTheReason(t *testing.T) {
	for _, msg := range []string{`hypopg: extension "hypopg" is not installed`,
		"hypopg: permission denied for function hypopg_hide_index"} {
		m := &fakeMeasurer{err: errors.New(msg)}
		pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
		cands, _, err := New(m).Generate(context.Background(), pack, env("db1"))
		if err != nil {
			t.Fatalf("a HypoPG error failed the generator: %v", err)
		}
		if len(cands) != 2 {
			t.Fatalf("%d candidates, want the 2 steps as advice", len(cands))
		}
		for _, c := range cands {
			if c.ActionType != candidate.ActionAdvice || !strings.Contains(c.Reason, msg) {
				t.Errorf("%s: %s reason %q", c.Target, c.ActionType, c.Reason)
			}
		}
	}
}

func TestGenerate_ACanceledContextIsAnErrorNotAdvice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &fakeMeasurer{err: context.Canceled}
	pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
	_, _, err := New(m).Generate(ctx, pack, env("db1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestGenerate_OldHypoPGWithoutHideIsAdvice(t *testing.T) {
	m := &fakeMeasurer{res: optimizer.WhatIfResult{OldIndexPresent: true}}
	pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
	cands, _, err := New(m).Generate(context.Background(), pack, env("db1"))
	if err != nil || len(cands) != 2 {
		t.Fatalf("%d candidates, err %v; want 2", len(cands), err)
	}
	for _, c := range cands {
		if c.ActionType != candidate.ActionAdvice || !strings.Contains(c.Reason, "hypopg") {
			t.Errorf("%s: %s reason %q", c.Target, c.ActionType, c.Reason)
		}
	}
}

func TestGenerate_UnplannedOrUnmeasuredSetIsAdvice(t *testing.T) {
	for _, res := range []optimizer.WhatIfResult{{Measured: 0},
		{Measured: 1, Failed: 1, UnplannedQueryIDs: []int64{7}}} {
		m := &fakeMeasurer{res: res}
		pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
		cands, _, err := New(m).Generate(context.Background(), pack, env("db1"))
		if err != nil || len(cands) != 2 {
			t.Fatalf("%+v: %d candidates, err %v", res, len(cands), err)
		}
		for _, c := range cands {
			if c.ActionType != candidate.ActionAdvice {
				t.Errorf("%+v: %s is %s, want advice", res, c.Target, c.ActionType)
			}
		}
	}
}

func TestGenerate_RegressionAboveTheBarDropsTheSet(t *testing.T) {
	m := &fakeMeasurer{res: optimizer.WhatIfResult{Measured: 2, Guarded: 2,
		Regressions: []optimizer.QueryRegression{{QueryID: 9, Pct: 12}}}}
	pack := mustPack(t, spec("db1", 90, "ok", chainFacts()...))
	cands, cov, err := New(m).Generate(context.Background(), pack, env("db1"))
	if err != nil || len(cands) != 0 {
		t.Fatalf("%d candidates (%+v), err %v; want none", len(cands), cands, err)
	}
	if !strings.Contains(strings.Join(cov.Notes, " "), "queryid 9") {
		t.Fatalf("coverage notes %v do not name queryid 9", cov.Notes)
	}
}
