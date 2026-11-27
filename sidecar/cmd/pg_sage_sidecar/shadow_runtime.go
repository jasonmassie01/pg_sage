package main

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Shadow mode (roadmap 1.4) wiring: each database's scorer scores its
// pending shadow decisions on the ledger's reconcile interval (before the
// reconciler copies the counted scores into the ledger), and /metrics
// exposes the shadow decision and score counters.

// shadowScorerOptions maps the operator's verification settings (what an
// externally applied change is verified with) and the optimizer's
// what-if bar onto the scorer; the shadow windows keep their defaults.
func shadowScorerOptions(c *config.Config, database string) shadow.Options {
	o := shadow.DefaultOptions()
	o.Database = database
	if c == nil {
		return o
	}
	v := c.Verify
	if v.WindowMinutes > 0 {
		o.VerifyWindow = time.Duration(v.WindowMinutes) * time.Minute
	}
	if v.WindowMaxMinutes > 0 {
		o.VerifyMaxWindow = time.Duration(v.WindowMaxMinutes) * time.Minute
	}
	if v.DropWindowHours > 0 {
		o.DropWindow = time.Duration(v.DropWindowHours) * time.Hour
	}
	o.Thresholds = verify.Thresholds{MinSamples: v.MinSamples, GainPct: v.MinGainPct,
		RegressPct: v.RegressPct}
	if c.LLM.Optimizer.HypoPGMinImprovePct > 0 {
		o.HypoPGMinPct = c.LLM.Optimizer.HypoPGMinImprovePct
	}
	return o
}

// startShadowScoring scores the database's shadow decisions on the
// reconcile interval.
func (rt *databaseRuntime) startShadowScoring() {
	name := rt.spec.Name
	scorer := shadow.NewScorer(rt.spec.Pool, shadowScorerOptions(rt.cfg, name),
		func(format string, args ...any) {
			logWarn("shadow", "db %q: "+format,
				append([]any{name}, args...)...)
		})
	rt.start(func() {
		every(rt.ctx, rt.cfg.SRE.Autonomy.ReconcileInterval(), func(ctx context.Context) {
			if _, err := scorer.RunOnce(ctx); err != nil {
				logWarn("shadow", "db %q: score shadow decisions: %v", name, err)
			}
		})
	})
	rt.note("shadow_mode")
}

// writeShadowMetrics renders the shadow counters.
func writeShadowMetrics(b *strings.Builder, decisions []shadow.DecisionCount,
	scores []shadow.ScoreCount) {
	metricsreg.Header(b, "pg_sage_shadow_decisions_total")
	for _, d := range decisions {
		writeSample(b, "pg_sage_shadow_decisions_total", []labelPair{{"database", d.Database},
			{"class", d.Class}, {"verdict", d.Verdict}}, d.Count)
	}
	metricsreg.Header(b, "pg_sage_shadow_scores_total")
	for _, s := range scores {
		writeSample(b, "pg_sage_shadow_scores_total", []labelPair{{"database", s.Database},
			{"class", s.Class}, {"score", s.Score}, {"source", s.Source}}, s.Count)
	}
	b.WriteString("\n")
}
