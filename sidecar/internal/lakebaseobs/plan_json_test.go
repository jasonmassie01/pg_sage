package lakebaseobs

import (
	"encoding/json"
	"testing"
)

func TestPlanHistoryJSONPreservesStructuredPlanAndUnknownFlags(t *testing.T) {
	off := false
	raw, err := json.Marshal(Plan{QueryID: 9, JSON: []byte(`[{"Plan":{"Node Type":"Result"}}]`), AnalyzeEnabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["JSON"].([]any); !ok {
		t.Fatal("captured plan encoded as opaque bytes")
	}
	if value["QueryID"] != float64(9) || value["AnalyzeEnabled"] != false || value["TimingEnabled"] != nil {
		t.Fatal("capture identity/unknown flags lost")
	}
}
