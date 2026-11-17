package policy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// guard-01a: the standing policy document's pressure block sets the slow
// and stop limits check_pressure answers against, and which inputs are
// required (an unknown required input is a stop). No concurrent access
// tests: parsing and marshalling are pure functions of their input.

func withPressure(t *testing.T, block string) []byte {
	t.Helper()
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("marshal staffed: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode staffed: %v", err)
	}
	wire["pressure"] = json.RawMessage(block)
	out, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func TestPressurePolicy_ParsesLimitsAndRequired(t *testing.T) {
	doc, err := ParseDocument(withPressure(t, `{"limits": {
		"replica_lag_seconds": {"slow": 4, "stop": 9},
		"free_storage_pct": {"stop": 15}},
		"required": ["replica_lag_seconds", "free_storage_pct"]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lag := doc.Pressure.Limits[PressureReplicaLag]
	if lag.Slow == nil || *lag.Slow != 4 || lag.Stop == nil || *lag.Stop != 9 {
		t.Fatalf("replica lag limit = %+v, want slow 4 stop 9", lag)
	}
	storage := doc.Pressure.Limits[PressureFreeStorage]
	if storage.Slow != nil || storage.Stop == nil || *storage.Stop != 15 {
		t.Fatalf("free storage limit = %+v, want only stop 15", storage)
	}
	if strings.Join(doc.Pressure.Required, ",") != "replica_lag_seconds,free_storage_pct" {
		t.Fatalf("required = %v", doc.Pressure.Required)
	}
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("reparse %s: %v", raw, err)
	}
	if got := again.Pressure.Limits[PressureReplicaLag]; got.Stop == nil || *got.Stop != 9 ||
		len(again.Pressure.Required) != 2 {
		t.Fatalf("round trip lost the block: %+v", again.Pressure)
	}
}

func TestPressurePolicy_AbsentBlockIsNotWritten(t *testing.T) {
	doc := StaffedProfile()
	if doc.Pressure.Limits != nil || doc.Pressure.Required != nil {
		t.Fatalf("default pressure block = %+v, want zero", doc.Pressure)
	}
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"pressure"`)) {
		t.Fatalf("a default pressure block was written: %s", raw)
	}
}

func TestPressurePolicy_EmptyRequiredListIsKept(t *testing.T) {
	doc, err := ParseDocument(withPressure(t, `{"required": []}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Pressure.Required == nil || len(doc.Pressure.Required) != 0 {
		t.Fatalf("required = %#v, want an explicit empty list (nothing required)",
			doc.Pressure.Required)
	}
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"required":[]`)) {
		t.Fatalf("an explicit empty required list was dropped: %s", raw)
	}
}

func TestPressurePolicy_RefusesInvalidBlocks(t *testing.T) {
	cases := map[string]string{
		"unknown input":       `{"limits": {"cpu": {"stop": 1}}}`,
		"unknown required":    `{"required": ["cpu"]}`,
		"duplicate required":  `{"required": ["xid_age", "xid_age"]}`,
		"negative":            `{"limits": {"lock_queue": {"slow": -1}}}`,
		"slow at stop":        `{"limits": {"replica_lag_seconds": {"slow": 9, "stop": 9}}}`,
		"slow above stop":     `{"limits": {"xid_age": {"slow": 10, "stop": 5}}}`,
		"storage slow below":  `{"limits": {"free_storage_pct": {"slow": 10, "stop": 20}}}`,
		"storage over 100":    `{"limits": {"free_storage_pct": {"stop": 101}}}`,
		"unknown field":       `{"limits": {"xid_age": {"warn": 1}}}`,
		"unknown block field": `{"mode": "enforce"}`,
		"empty limit":         `{"limits": {"xid_age": {}}}`,
	}
	for name, block := range cases {
		if _, err := ParseDocument(withPressure(t, block)); err == nil {
			t.Errorf("%s: %s parsed, want an error", name, block)
		} else if !strings.Contains(err.Error(), "pressure") {
			t.Errorf("%s: error %q does not name the pressure block", name, err)
		}
	}
}

func TestPressurePolicy_BoundaryValuesAreAccepted(t *testing.T) {
	doc, err := ParseDocument(withPressure(t, `{"limits": {
		"lock_queue": {"slow": 0, "stop": 1},
		"free_storage_pct": {"slow": 100, "stop": 0}}}`))
	if err != nil {
		t.Fatalf("parse boundary values: %v", err)
	}
	if got := doc.Pressure.Limits[PressureLockQueue]; got.Slow == nil || *got.Slow != 0 {
		t.Fatalf("an explicit slow of 0 was lost: %+v", got)
	}
}

func TestPressureInputs_AreTheDocumentedSevenInOrder(t *testing.T) {
	want := "replica_lag_seconds,xid_age,multixact_age,vacuum_running," +
		"wal_bytes_per_second,lock_queue,free_storage_pct"
	if got := strings.Join(PressureInputs(), ","); got != want {
		t.Fatalf("inputs = %s, want %s", got, want)
	}
	for _, name := range PressureInputs() {
		if PressureLowerIsWorse(name) != (name == PressureFreeStorage) {
			t.Errorf("%s lower-is-worse = %v", name, PressureLowerIsWorse(name))
		}
	}
}
