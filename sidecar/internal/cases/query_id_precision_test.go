package cases

import (
	"encoding/json"
	"math"
	"testing"
)

func TestDetailQueryIDPrecision(t *testing.T) {
	for _, tc := range []struct {
		name, object, want string
		detail             map[string]any
	}{
		{"negative_legacy_float", "queryid:-5191732810777595558", "-5191732810777595558", map[string]any{"queryid": float64(-5191732810777595558)}},
		{"positive_legacy_float", "queryid:8379109690210330486", "8379109690210330486", map[string]any{"queryid": float64(8379109690210330486)}},
		{"signed_min_string", "queryid:-9223372036854775808", "-9223372036854775808", map[string]any{"queryid": "-9223372036854775808"}},
		{"signed_max_int64", "queryid:9223372036854775807", "9223372036854775807", map[string]any{"queryid": int64(9223372036854775807)}},
		{"json_number", "queryid:8379109690210330486", "8379109690210330486", map[string]any{"queryid": json.Number("8379109690210330486")}},
		{"legacy_safe_float", "queryid:42", "42", map[string]any{"queryid": float64(42)}},
		{"legacy_int", "queryid:42", "42", map[string]any{"queryid": int(42)}},
		{"missing_detail", "queryid:-5191732810777595558", "-5191732810777595558", nil},
		{"float_without_identity", "", "", map[string]any{"queryid": float64(-5191732810777595558)}},
		{"conflicting_float", "queryid:42", "", map[string]any{"queryid": float64(-5191732810777595558)}},
		{"conflicting_string", "queryid:42", "", map[string]any{"queryid": "41"}},
		{"explicit_nil", "queryid:42", "", map[string]any{"queryid": nil}},
		{"explicit_empty", "queryid:42", "", map[string]any{"queryid": ""}},
		{"fractional", "queryid:42", "", map[string]any{"queryid": 42.5}},
		{"nan", "queryid:42", "", map[string]any{"queryid": math.NaN()}},
		{"infinity", "queryid:42", "", map[string]any{"queryid": math.Inf(1)}},
		{"overflow_identity", "queryid:9223372036854775808", "", map[string]any{"queryid": float64(42)}},
		{"noncanonical_identity", "queryid:0042", "", map[string]any{"queryid": float64(42)}},
		{"invalid_string", "queryid:42", "", map[string]any{"queryid": "42; DROP TABLE orders"}},
		{"overflow_string", "queryid:42", "", map[string]any{"queryid": "9223372036854775808"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detailQueryID(SourceFinding{ObjectIdentifier: tc.object, Detail: tc.detail})
			if got != tc.want {
				t.Fatalf("queryid=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestQueryArtifactsOmitUnverifiedIDs(t *testing.T) {
	for _, category := range []string{"slow_query", "query_tuning"} {
		t.Run(category, func(t *testing.T) {
			f := SourceFinding{ID: "123", Category: category, ObjectType: "query", ObjectIdentifier: "queryid:42",
				RecommendedSQL: "SELECT 'stored proposal'", Detail: map[string]any{"queryid": "41", "query": "SELECT payload FROM orders"}}
			projected := ProjectFinding(f)
			if len(projected.ActionCandidates) != 1 || projected.ActionCandidates[0].ScriptOutput != nil {
				t.Fatalf("unverified query ID generated an artifact: %+v", projected.ActionCandidates)
			}
		})
	}
}
