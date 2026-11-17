package connections

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// webMix is web: mean 5 active (peak 8), 30 idle, 2 idle in transaction
// and 1 aborted; api: mean 4 active, 1 idle.
func webMix() []AppState {
	return []AppState{
		{Application: "web", State: "active", Mean: 5, Peak: 8, PeakAt: t0},
		{Application: "web", State: "idle", Mean: 30, Peak: 40, PeakAt: t0},
		{Application: "web", State: "idle in transaction", Mean: 2, Peak: 3, PeakAt: t0},
		{Application: "web", State: "idle in transaction (aborted)", Mean: 1, Peak: 1,
			PeakAt: t0},
		{Application: "api", State: "active", Mean: 4, Peak: 6, PeakAt: t0},
		{Application: "api", State: "idle", Mean: 1, Peak: 2, PeakAt: t0},
	}
}

// The per-application state mix: active, idle and idle in transaction
// (aborted included) means, in application order.
func TestMixes_PerApplicationStateMix(t *testing.T) {
	m := Mixes(Inputs{Mix: webMix()})
	if len(m) != 2 || m[0].Application != "api" || m[1].Application != "web" {
		t.Fatalf("mixes %+v", m)
	}
	w := m[1]
	if w.Active != 5 || w.Idle != 30 || w.IdleInTx != 3 || w.PeakActive != 8 ||
		!w.PeakActiveAt.Equal(t0) {
		t.Fatalf("web mix %+v", w)
	}
	if got := w.IdleShare(); got < 0.789 || got > 0.790 {
		t.Fatalf("web idle share %v, want 30/38", got)
	}
	if got := Mixes(Inputs{}); len(got) != 0 {
		t.Fatalf("empty mix %+v", got)
	}
}

// An application mostly idle gets pooler advice as text: transaction mode
// sized from its active peak plus 20%, never applied by pg_sage.
func TestBuild_IdleHeavyApplicationGetsPoolerAdvice(t *testing.T) {
	cands, _ := build(t, plainPack(t), Inputs{Mix: webMix()})
	c := only(t, cands, KindPoolerSizing)
	if c.Target != "application:web" || c.ActionType != candidate.ActionAdvice ||
		c.ForwardSQL != "" {
		t.Fatalf("pooler %+v", c)
	}
	if c.Prediction.Quantities[QuantityPoolSize] != 10 {
		t.Fatalf("pool size %v, want ceil(8 * 1.2) = 10", c.Prediction.Quantities)
	}
	for _, want := range []string{"transaction", "pool of 10", "never changes"} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, c.Reason)
		}
	}
	if len(c.EvidenceIDs) != 1 || c.EvidenceIDs[0] != hourID(t0Hour) {
		t.Fatalf("evidence %v", c.EvidenceIDs)
	}
}

// Boundary: half idle gets advice, under half does not; an application
// under PoolerMinSessions mean sessions never does.
func TestBuild_PoolerAdviceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		active, idle float64
		advice       bool
	}{{10, 10, true}, {10.2, 9.8, false}, {4, 5.9, false}, {4, 6, true}} {
		mix := []AppState{{Application: "w", State: "active", Mean: tc.active, Peak: 12,
			PeakAt: t0}, {Application: "w", State: "idle", Mean: tc.idle, Peak: 12,
			PeakAt: t0}}
		cands, _ := build(t, plainPack(t), Inputs{Mix: mix})
		if got := len(ofKind(cands, KindPoolerSizing)) == 1; got != tc.advice {
			t.Errorf("active %v idle %v: advice %v, want %v", tc.active, tc.idle, got,
				tc.advice)
		}
	}
}
