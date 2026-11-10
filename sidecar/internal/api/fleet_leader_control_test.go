package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/leader"
)

// fleet-03 CHECK-24: the leader route names the control database. No DB
// or concurrency category: the handler only serializes a status value.

func TestFleetLeaderHandler_ReportsTheControlDatabase(t *testing.T) {
	reader := &fakeFleetLearning{status: leader.Status{Enabled: true, Leader: true,
		Holder: "host-1", ControlDatname: "orders",
		ControlSystemIdentifier: "7312345678901234567"}}
	body := getFleetFindings(t, fleetLeaderHandler(reader), "/api/v1/fleet/leader",
		http.StatusOK)
	if body["control_datname"] != "orders" {
		t.Fatalf("control_datname = %v, want orders", body["control_datname"])
	}
	// A string, so a JavaScript reader keeps all 19 digits.
	if body["control_system_identifier"] != "7312345678901234567" {
		t.Fatalf("control_system_identifier = %#v, want the exact decimal string",
			body["control_system_identifier"])
	}
	if body["leadership_refused"] != false {
		t.Fatalf("leadership_refused = %v, want false", body["leadership_refused"])
	}
}

func TestFleetLeaderHandler_RestrictedIdentifierIsOmittedAndRefusalShown(t *testing.T) {
	reader := &fakeFleetLearning{status: leader.Status{Enabled: true,
		ControlDatname: "orders", LeadershipRefused: true,
		RefusedReason: "another live holder"}}
	body := getFleetFindings(t, fleetLeaderHandler(reader), "/api/v1/fleet/leader",
		http.StatusOK)
	if _, ok := body["control_system_identifier"]; ok {
		t.Fatalf("restricted identifier must be omitted, body = %v", body)
	}
	if body["control_datname"] != "orders" || body["leadership_refused"] != true ||
		body["refused_reason"] != "another live holder" || body["leader"] != false {
		t.Fatalf("status = %v, want datname, refusal and no leader", body)
	}
}
