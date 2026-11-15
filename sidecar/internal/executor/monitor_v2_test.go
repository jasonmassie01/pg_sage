package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-01c: the post-action monitor computes v2 beside v1 and stores it as
// the other verdict (other_verdict, other_evidence). It acts on v1 in
// every mode: v2 decides the monitor's rollbacks only once lab-02's
// back-test lets auto choose v2.

type monitorCounters struct {
	step time.Time
	err  error
}

// Window is 24 five-minute buckets of query 1 (100 blocks per call
// before, 60 after) and the flat controls 11-13.
func (c monitorCounters) Window(_ context.Context, ids []int64, from, to time.Time,
	_ time.Duration) (verify.CounterWindow, error) {
	if c.err != nil {
		return verify.CounterWindow{}, c.err
	}
	post := to.After(c.step)
	out := verify.Series{}
	for _, id := range ids {
		blocks := 100.0
		if id == 1 && post {
			blocks = 60
		}
		for i := 0; i < 24; i++ {
			w := 1 + 0.02*float64(i%3-1)
			out[id] = append(out[id], verify.Bucket{
				Start: from.Add(time.Duration(i) * 5 * time.Minute), Calls: 50,
				Blocks: blocks * w * 50, TotalMs: 4 * w * 50, Rows: 50})
		}
	}
	return verify.CounterWindow{Series: out}, nil
}

func (c monitorCounters) Seasonal(context.Context, []int64, time.Time, time.Time, int) (
	verify.Series, error) {
	return verify.Series{}, nil
}

type monitorMechanisms struct{}

func (monitorMechanisms) CheckMechanism(_ context.Context, in verify.MechanismInput,
	_ time.Time) (verify.Mechanism, error) {
	return verify.Mechanism{Counted: in.Targets}, nil
}

func monitorV2Config(method string, err error, step time.Time) RollbackMonitorConfig {
	return RollbackMonitorConfig{GainPct: 20, ThresholdPct: 15, V2: &verify.V2Options{
		Counters: monitorCounters{step: step, err: err}, Mechanisms: monitorMechanisms{},
		Method: func(context.Context) string { return method }, Cycle: 5 * time.Minute,
		Controls: func(context.Context, verify.WatchState) ([]int64, error) {
			return []int64{11, 12, 13}, nil
		}}}
}

func monitorV2Plan(class string, step time.Time) monitorPlan {
	return monitorPlan{actionID: 5, class: class, executedAt: step, targets: []int64{1},
		index: "public.orders_status_idx"}
}

func v1Outcome() verify.Outcome {
	return verify.Outcome{Verdict: verify.OutcomeNeutral, Reason: "v1 reason",
		Evidence: map[string]any{}}
}

func TestV2_MonitorShadowStoresTheV2Verdict(t *testing.T) {
	step := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, method := range []string{verify.MethodShadow, verify.MethodV2} {
		o := v1Outcome()
		monitorV2Plan(verify.ClassIndexDrop, step).shadowV2(context.Background(),
			monitorV2Config(method, nil, step), step.Add(2*time.Hour), &o)
		if o.Verdict != verify.OutcomeNeutral || o.Reason != "v1 reason" ||
			o.Verifier != verify.VerifierV1 {
			t.Fatalf("%s: the monitor acted on %+v, want v1's verdict", method, o)
		}
		if !verify.DecidedVerdict(o.OtherVerdict) {
			t.Fatalf("%s: other verdict %q, want a decided v2 verdict", method,
				o.OtherVerdict)
		}
		rec, _ := o.OtherEvidence["v2"].(map[string]any)
		if rec["verifier"] != verify.VerifierV2 || o.Evidence["v2"] == nil {
			t.Fatalf("%s: v2 record %v (acted evidence %v)", method, rec, o.Evidence)
		}
	}
}

func TestV2_MonitorV1ComputesNothing(t *testing.T) {
	step := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	o := v1Outcome()
	monitorV2Plan(verify.ClassIndexDrop, step).shadowV2(context.Background(),
		monitorV2Config(verify.MethodV1, nil, step), step.Add(2*time.Hour), &o)
	if o.Verifier != verify.VerifierV1 || o.OtherVerdict != "" || o.OtherEvidence != nil {
		t.Fatalf("v1: %+v, want no other verdict", o)
	}
	o = v1Outcome()
	monitorV2Plan(verify.ClassVacuum, step).shadowV2(context.Background(),
		monitorV2Config(verify.MethodShadow, nil, step), step.Add(2*time.Hour), &o)
	if o.OtherVerdict != "" {
		t.Fatalf("VACUUM has no v2 judgement, got %+v", o)
	}
	o = v1Outcome()
	monitorV2Plan(verify.ClassIndexDrop, step).shadowV2(context.Background(),
		RollbackMonitorConfig{}, step.Add(2*time.Hour), &o)
	if o.Verifier != verify.VerifierV1 || o.OtherVerdict != "" {
		t.Fatalf("no v2 wiring: %+v", o)
	}
}

func TestV2_MonitorReadFailureIsRecorded(t *testing.T) {
	step := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	o := v1Outcome()
	monitorV2Plan(verify.ClassIndexDrop, step).shadowV2(context.Background(),
		monitorV2Config(verify.MethodShadow, errors.New("snapshots unreadable"), step),
		step.Add(2*time.Hour), &o)
	rec, _ := o.OtherEvidence["v2"].(map[string]any)
	reason, _ := rec["reason"].(string)
	if o.Verdict != verify.OutcomeNeutral || o.OtherVerdict != verify.OutcomeInsufficient ||
		!strings.Contains(reason, "snapshots unreadable") {
		t.Fatalf("read failure: %+v, want v1 acting and an insufficient v2 naming the cause",
			o)
	}
}
