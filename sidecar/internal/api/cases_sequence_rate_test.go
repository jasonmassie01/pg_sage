package api

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// playbooks-03b without a database: the projection stays pure, so a path
// that never attaches the runway (policy explain) still gets a script
// that says the rate is not measured. No concurrent access tests: the
// functions keep no state.

func sequenceRow(ident string, detail any) map[string]any {
	return map[string]any{"id": "7", "database_name": "prod",
		"category": "sequence_exhaustion", "severity": "critical", "object_type": "sequence",
		"object_identifier": ident, "title": "sequence 91% consumed", "detail": detail}
}

// Unit: projectFindingRows without attachSequenceRunway.
func TestSequenceRateProjectionWithoutAttach(t *testing.T) {
	out, ids := projectFindingRows([]map[string]any{
		sequenceRow("public.orders_id_seq", map[string]any{"data_type": "integer"})})
	if len(out) != 1 || len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("projected %d cases, ids %v", len(out), ids)
	}
	script := out[0].ActionCandidates[0].ScriptOutput
	if script == nil || !strings.Contains(script.MigrationSQL,
		"-- Rate: not measured yet (no runway forecast for this sequence).") {
		t.Fatalf("script = %+v", script)
	}
}

// Nil/empty: a nil pool or rows with no sequence finding read nothing and
// change nothing.
func TestSequenceRateAttachWithoutPoolOrSequences(t *testing.T) {
	rows := []map[string]any{
		sequenceRow("public.orders_id_seq", map[string]any{"data_type": "integer"}),
		{"category": "stale_statistics", "object_identifier": "public.t",
			"detail": map[string]any{}},
	}
	want := []map[string]any{
		sequenceRow("public.orders_id_seq", map[string]any{"data_type": "integer"}),
		{"category": "stale_statistics", "object_identifier": "public.t",
			"detail": map[string]any{}},
	}
	attachSequenceRunway(context.Background(), nil, "prod", rows)
	attachSequenceRunway(context.Background(), nil, "prod", nil)
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows changed: %v", rows)
	}
}

// Invalid input: only sequence rows with a name and a detail object are
// looked up, each name once.
func TestSequenceRateLookupNames(t *testing.T) {
	rows := []map[string]any{
		sequenceRow("public.a_seq", map[string]any{}),
		sequenceRow(" public.a_seq ", nil),
		sequenceRow("", map[string]any{}),
		sequenceRow("   ", map[string]any{}),
		sequenceRow("public.raw_seq", "not an object"),
		{"category": "forecast_sequence_runway", "object_identifier": "public.b_seq",
			"detail": map[string]any{}},
	}
	got := sequenceRunwayNames(rows)
	if !reflect.DeepEqual(got, []string{"public.a_seq"}) {
		t.Fatalf("names = %v, want [public.a_seq]", got)
	}
}
