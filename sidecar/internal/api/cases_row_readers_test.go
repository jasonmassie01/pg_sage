package api

import (
	"testing"
	"time"
)

// cases-30: the generic row readers keep today's results after the split.
// No concurrency or state tests: the readers are pure functions of one
// value. No invalid-input path beyond the wrong-type cases below: the
// readers take whatever a scanned row holds.

func TestFloatValue_PinsTodaysConversions(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
	}{
		{"nil", nil, 0},
		{"string", "x", 0},
		{"numeric string is not parsed", "1.5", 0},
		{"float64", 0.75, 0.75},
		{"float32", float32(0.5), 0.5},
		{"int", 7, 7},
		{"int64", int64(1) << 40, float64(int64(1) << 40)},
		{"int32", int32(-3), -3},
		{"bool", true, 0},
	}
	for _, tc := range cases {
		if got := floatValue(tc.in); got != tc.want {
			t.Errorf("floatValue(%s %#v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestFloatPtrFromAny_NilOnlyForNil(t *testing.T) {
	if got := floatPtrFromAny(nil); got != nil {
		t.Fatalf("floatPtrFromAny(nil) = %v, want nil", *got)
	}
	got := floatPtrFromAny("x")
	if got == nil || *got != 0 {
		t.Fatalf("floatPtrFromAny(\"x\") = %v, want a pointer to 0", got)
	}
	got = floatPtrFromAny(int64(12))
	if got == nil || *got != 12 {
		t.Fatalf("floatPtrFromAny(12) = %v, want a pointer to 12", got)
	}
}

func TestTimePtrFromMap_PinsTodaysResults(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var nilTime *time.Time
	row := map[string]any{
		"wrong_type": "2026-01-02T03:04:05Z",
		"zero":       time.Time{},
		"value":      at,
		"pointer":    &at,
		"nil_ptr":    nilTime,
	}
	for _, key := range []string{"missing", "wrong_type", "zero", "nil_ptr"} {
		if got := timePtrFromMap(row, key); got != nil {
			t.Errorf("timePtrFromMap(%q) = %v, want nil", key, *got)
		}
	}
	for _, key := range []string{"value", "pointer"} {
		got := timePtrFromMap(row, key)
		if got == nil || !got.Equal(at) {
			t.Fatalf("timePtrFromMap(%q) = %v, want %v", key, got, at)
		}
	}
	if got := timePtrFromMap(nil, "value"); got != nil {
		t.Fatalf("timePtrFromMap(nil row) = %v, want nil", *got)
	}
}

func TestStringValueAndDetailMap_PinTodaysResults(t *testing.T) {
	if got := stringValue(nil); got != "" {
		t.Fatalf("stringValue(nil) = %q, want empty", got)
	}
	if got := stringValue(int64(42)); got != "42" {
		t.Fatalf("stringValue(42) = %q, want \"42\"", got)
	}
	if got := detailMap(nil); got != nil {
		t.Fatalf("detailMap(nil) = %v, want nil", got)
	}
	raw := detailMap("not a map")
	if len(raw) != 1 || raw["raw"] != "not a map" {
		t.Fatalf("detailMap(string) = %v, want {raw: not a map}", raw)
	}
	m := map[string]any{"k": 1}
	if got := detailMap(m); len(got) != 1 || got["k"] != 1 {
		t.Fatalf("detailMap(map) = %v, want the same map", got)
	}
}

func TestStringSliceFromAny_PinsTodaysResults(t *testing.T) {
	if got := stringSliceFromAny(nil); got != nil {
		t.Fatalf("stringSliceFromAny(nil) = %v, want nil", got)
	}
	src := []string{"a", "b"}
	got := stringSliceFromAny(src)
	src[0] = "changed"
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("stringSliceFromAny([]string) = %v, want a copy [a b]", got)
	}
	mixed := stringSliceFromAny([]any{"x", nil, "", 3})
	if len(mixed) != 2 || mixed[0] != "x" || mixed[1] != "3" {
		t.Fatalf("stringSliceFromAny([]any) = %v, want [x 3]", mixed)
	}
	if got := stringSliceFromAny(42); got != nil {
		t.Fatalf("stringSliceFromAny(42) = %v, want nil", got)
	}
}

func TestSourceQueryHintFromMap_ReadsEveryField(t *testing.T) {
	created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	verified := created.Add(time.Hour)
	before, after := 120.0, 30.0
	got := sourceQueryHintFromMap(map[string]any{
		"queryid": int64(9007199254740993), "database_name": "prod",
		"hint_text": "IndexScan(t)", "symptom": "seq_scan", "status": "active",
		"created_at": created, "before_cost": before, "after_cost": &after,
		"suggested_rewrite": "SELECT 1", "rewrite_rationale": "why",
		"verified_at": verified,
	})
	if got.QueryID != 9007199254740993 || got.DatabaseName != "prod" ||
		got.HintText != "IndexScan(t)" || got.Symptom != "seq_scan" ||
		got.Status != "active" || !got.CreatedAt.Equal(created) ||
		got.SuggestedRewrite != "SELECT 1" || got.RewriteRationale != "why" {
		t.Fatalf("sourceQueryHintFromMap scalars = %+v", got)
	}
	if got.BeforeCost == nil || *got.BeforeCost != before {
		t.Fatalf("BeforeCost = %v, want %v", got.BeforeCost, before)
	}
	// A *float64 is not one of floatValue's types, so today it reads as 0.
	// scanQueryHintRows stores both costs as *float64: a defect reported
	// in cases-30 and left for its own fix, since this split changes no
	// behaviour.
	if got.AfterCost == nil || *got.AfterCost != 0 {
		t.Fatalf("AfterCost = %v, want a pointer to 0", got.AfterCost)
	}
	if got.VerifiedAt == nil || !got.VerifiedAt.Equal(verified) || got.RolledBackAt != nil {
		t.Fatalf("VerifiedAt / RolledBackAt = %v / %v", got.VerifiedAt, got.RolledBackAt)
	}
}
