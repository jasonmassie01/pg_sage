package executor

import (
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// The tuning agent writes target queryids as decimal strings so they
// survive the finding detail's JSONB round trip (numbers decode as
// float64, which cannot hold every int64 queryid).
func TestPredictionProducerStringQueryIDsAreExact(t *testing.T) {
	detail := map[string]any{"predicted_effect": map[string]any{
		"method": "hypopg", "metric": "mean_exec_time", "expected_change_pct": -40.0,
		"source":          "tuning_agent",
		"target_queryids": []string{"-3534472208683223210", "9223372036854775807"}}}
	var roundTrip map[string]any
	raw, _ := json.Marshal(detail)
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	for name, d := range map[string]map[string]any{"in memory": detail,
		"after JSONB": roundTrip} {
		p := predictionFromDetail(verify.ClassIndexCreate, d)
		if p.Source != "tuning_agent" || len(p.TargetQueryIDs) != 2 ||
			p.TargetQueryIDs[0] != -3534472208683223210 ||
			p.TargetQueryIDs[1] != 9223372036854775807 {
			t.Fatalf("%s: prediction = %+v", name, p)
		}
	}
	bad := map[string]any{"predicted_effect": map[string]any{"method": "model",
		"expected_change_pct": -10.0, "target_queryids": []any{"12x"}}}
	if p := predictionFromDetail(verify.ClassIndexCreate, bad); p.Source == "tuning_agent" ||
		len(p.TargetQueryIDs) != 0 {
		t.Fatalf("a malformed queryid voids the producer's prediction: %+v", p)
	}
}
