package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// TestIsQueryRegressed covers the pure F1 regression decision.
func TestIsQueryRegressed(t *testing.T) {
	cases := []struct {
		name              string
		baseline, current float64
		threshold         int
		want              bool
	}{
		{"clear regression", 10, 15, 20, true},   // +50%
		{"within threshold", 10, 11, 20, false},  // +10%
		{"improved", 10, 5, 20, false},           // -50%
		{"exactly at threshold", 10, 12, 20, false}, // +20% not > 20
		{"just over threshold", 10, 12.1, 20, true},
		{"zero baseline", 0, 100, 20, false},     // can't compute
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isQueryRegressed(c.baseline, c.current, c.threshold); got != c.want {
				t.Errorf("isQueryRegressed(%v,%v,%d) = %v, want %v",
					c.baseline, c.current, c.threshold, got, c.want)
			}
		})
	}
}

// TestTargetQueryIDs covers extraction from a finding's detail, including
// the JSON-float and list cases.
func TestTargetQueryIDs(t *testing.T) {
	if got := targetQueryIDs(analyzer.Finding{}); got != nil {
		t.Errorf("nil detail: got %v, want nil", got)
	}
	if got := targetQueryIDs(analyzer.Finding{
		Detail: map[string]any{"queryid": int64(123)},
	}); len(got) != 1 || got[0] != 123 {
		t.Errorf("int64 queryid: got %v", got)
	}
	if got := targetQueryIDs(analyzer.Finding{
		Detail: map[string]any{"queryid": float64(456)},
	}); len(got) != 1 || got[0] != 456 {
		t.Errorf("float64 queryid (JSON): got %v", got)
	}
	if got := targetQueryIDs(analyzer.Finding{
		Detail: map[string]any{"queryids": []any{float64(1), float64(2)}},
	}); len(got) != 2 {
		t.Errorf("queryids list: got %v", got)
	}
	if got := targetQueryIDs(analyzer.Finding{
		Detail: map[string]any{"table": "public.orders"},
	}); got != nil {
		t.Errorf("no queryid: got %v, want nil", got)
	}
}

// TestTargetQueryIDs_Int64List covers the in-memory []int64 case from the
// optimizer (A2), distinct from the JSON []any case.
func TestTargetQueryIDs_Int64List(t *testing.T) {
	got := targetQueryIDs(analyzer.Finding{
		Detail: map[string]any{"queryids": []int64{11, 22, 33}},
	})
	if len(got) != 3 || got[0] != 11 || got[2] != 33 {
		t.Errorf("[]int64 queryids: got %v", got)
	}
}
