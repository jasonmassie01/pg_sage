package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

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
