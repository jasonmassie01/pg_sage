package executor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// monitorV2Class is a monitored class's mechanism class and attribution
// kind for verification v2 (lab-01c).
type monitorV2Class struct{ mechanism, kind string }

// monitorV2Classes are the monitored classes v2 judges: a drop by its
// targets' plans and untouched controls, a server setting with no control
// group, a table's storage parameters by its outcome metric.
var monitorV2Classes = map[string]monitorV2Class{
	verify.ClassIndexDrop: {verify.MechanismIndexDrop, verify.AttributionIndex},
	verify.ClassGUC:       {verify.MechanismConfigGUC, verify.AttributionGlobal},
	verify.ClassReloption: {verify.MechanismAutovacuum, verify.AttributionIndex},
}

// shadowV2 computes v2 beside the monitor's v1 verdict o and stores it as
// the other verdict. The monitor acts on v1 in every mode: it has no
// durable look state, so each check is judged as a single look at the full
// alpha, which is fit for comparison only. A failed v2 read is recorded as
// an insufficient v2 verdict naming its cause.
func (p monitorPlan) shadowV2(ctx context.Context, cfg RollbackMonitorConfig, now time.Time,
	o *verify.Outcome) {
	o.Verifier = verify.VerifierV1
	class, ok := monitorV2Classes[p.class]
	if cfg.V2 == nil || cfg.V2.Method == nil || !ok || len(p.targets) == 0 ||
		cfg.V2.Method(ctx) == verify.MethodV1 {
		return
	}
	window := max(now.Sub(p.executedAt), time.Minute)
	metric := ""
	if p.config != nil {
		metric = p.config.Metric
	}
	state := verify.WatchState{ActionID: p.actionID, ExecutedAt: p.executedAt,
		Window: window, IndexName: p.index, Criterion: verify.Criterion{
			TargetIDs: p.targets, MinGainPct: cfg.GainPct,
			RegressPct: float64(cfg.ThresholdPct), Window: window, HardMax: window}}
	v, _, err := verify.JudgeLookV2(ctx, *cfg.V2, verify.LookV2{State: state, Now: now,
		Class: class.mechanism, Kind: class.kind, Metric: metric})
	if err != nil {
		reason := "v2 unavailable: " + err.Error()
		v = verify.Verdict{Outcome: verify.OutcomeInsufficient, Reason: reason,
			Evidence: map[string]any{"v2": map[string]any{"verifier": verify.VerifierV2,
				"outcome": verify.OutcomeInsufficient, "reason": reason}}}
	}
	if verify.DecidedVerdict(v.Outcome) {
		o.OtherVerdict = v.Outcome
	}
	o.OtherEvidence = v.Evidence
	if o.Evidence == nil {
		o.Evidence = map[string]any{}
	}
	o.Evidence["v2"] = v.Evidence["v2"]
}
