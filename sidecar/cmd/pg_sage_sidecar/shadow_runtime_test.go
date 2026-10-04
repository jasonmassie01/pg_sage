package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/shadow"
)

// Roadmap 1.4: each database's shadow scorer verifies externally applied
// changes with the operator's verification settings and judges index
// creates with the optimizer's what-if bar; /metrics exposes the shadow
// decision and score counters.

func TestShadowScorerOptionsFollowTheConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Verify.WindowMinutes, cfg.Verify.WindowMaxMinutes = 45, 600
	cfg.Verify.DropWindowHours = 12
	cfg.Verify.MinGainPct, cfg.Verify.RegressPct, cfg.Verify.MinSamples = 25, 12, 40
	cfg.Optimizer.HypoPGMinImprovePct = 15
	o := shadowScorerOptions(cfg, "orders")
	if o.Database != "orders" || o.VerifyWindow != 45*time.Minute ||
		o.VerifyMaxWindow != 600*time.Minute || o.DropWindow != 12*time.Hour ||
		o.HypoPGMinPct != 15 {
		t.Fatalf("options = %+v", o)
	}
	th := o.Thresholds
	if th.GainPct != 25 || th.RegressPct != 12 || th.MinSamples != 40 {
		t.Fatalf("thresholds = %+v", th)
	}
	def := shadow.DefaultOptions()
	if o.ScoreAfter != def.ScoreAfter || o.Horizon != def.Horizon ||
		o.DedupeWindow != def.DedupeWindow {
		t.Fatalf("shadow windows must keep their defaults: %+v", o)
	}
	if got := shadowScorerOptions(nil, "x"); got.Database != "x" || got.VerifyWindow <= 0 {
		t.Fatalf("nil config: %+v", got)
	}
}

func TestWriteShadowMetrics(t *testing.T) {
	var b strings.Builder
	writeShadowMetrics(&b,
		[]shadow.DecisionCount{{Database: `or"ders`, Class: "vacuum", Verdict: "execute",
			Count: 3}},
		[]shadow.ScoreCount{{Database: "orders", Class: "index_create", Score: "correct",
			Source: "hypopg", Count: 2}})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_shadow_decisions_total counter",
		`pg_sage_shadow_decisions_total{database="or\"ders",class="vacuum",verdict="execute"} 3`,
		"# TYPE pg_sage_shadow_scores_total counter",
		`pg_sage_shadow_scores_total{database="orders",class="index_create",score="correct",` +
			`source="hypopg"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	var empty strings.Builder
	writeShadowMetrics(&empty, nil, nil)
	if !strings.Contains(empty.String(), "# TYPE pg_sage_shadow_decisions_total counter") ||
		strings.Contains(empty.String(), "{") {
		t.Fatalf("no counters yet: %q", empty.String())
	}
}
