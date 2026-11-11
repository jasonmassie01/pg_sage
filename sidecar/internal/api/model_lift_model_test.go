package api

import (
	"strings"
	"testing"
	"time"
)

// llm-01 (CHECK-16): GET /api/v1/model-lift shows, per family, the model
// the deciding measurement was taken with and the model the investigator
// runs, and why a mismatch keeps the family advisory.

func TestModelLiftAPI_ShowsTheMeasuredAndRunningModel(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	f.useReleaseBuild()
	f.ingestSignedLift(liftReportJSON(f.clock.Now().Add(-time.Hour), "lock_blocking", 16, 16))
	f.ledger.WithRunningModel(func() string { return "Ollama/Llama3 " })
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET", "/api/v1/model-lift", "")
	if code != 200 {
		t.Fatalf("model lift = %d %v", code, body)
	}
	lock := familyLift(t, body, "lock_blocking")
	reason, _ := lock["reason"].(string)
	if lock["measured_model"] != "gpt-4o-mini" || lock["running_model"] != "llama3" ||
		lock["granted"] != false || lock["status"] != "advisory" ||
		!strings.Contains(reason, `measured with "gpt-4o-mini"; the investigator runs "llama3"`) {
		t.Fatalf("lock_blocking = %v", lock)
	}
	wal := familyLift(t, body, "wal_retention")
	if wal["measured_model"] != "" || wal["running_model"] != "llama3" {
		t.Fatalf("wal_retention = %v", wal)
	}
	f.ledger.WithRunningModel(nil)
	_, body = autonomyCall(t, f.router(testViewerUser()), "GET", "/api/v1/model-lift", "")
	lock = familyLift(t, body, "lock_blocking")
	reason, _ = lock["reason"].(string)
	if lock["granted"] != false || lock["running_model"] != "" ||
		!strings.Contains(reason, "the investigator's model is unknown") {
		t.Fatalf("no running model: lock_blocking = %v", lock)
	}
}
