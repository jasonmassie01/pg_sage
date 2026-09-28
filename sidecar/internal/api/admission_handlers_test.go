package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/verify"
)

func admissionManager(t *testing.T) *fleet.DatabaseManager {
	t.Helper()
	cfg := caseAutonomousConfig()
	mgr := fleet.NewManager(cfg)
	for _, name := range []string{"primary", "analytics"} {
		exec := caseGateExecutor(cfg)
		exec.WithDatabaseName(name)
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Config: config.DatabaseConfig{Name: name}, Executor: exec,
			Status: &fleet.InstanceStatus{Connected: true},
		})
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "stopped", Config: config.DatabaseConfig{Name: "stopped"},
		Status: &fleet.InstanceStatus{},
	})
	return mgr
}

func getAdmission(
	t *testing.T, handler http.HandlerFunc, target, name string,
) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if name != "" {
		req.SetPathValue("name", name)
	}
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAdmissionEndpointReportsReason(t *testing.T) {
	rec := getAdmission(t, admissionStatusHandler(admissionManager(t)),
		"/api/v1/admission/primary", "primary")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body executor.IndexAdmissionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.OK || body.Reason != verify.ReasonLoadUnavailable ||
		body.Mode != verify.EvidenceUnavailable || body.Database != "primary" {
		t.Fatalf("admission = %+v", body)
	}
	for _, missing := range []string{"pg_io_rate", "host_cpu"} {
		if !slices.Contains(body.MissingEvidence, missing) {
			t.Fatalf("missing_evidence %v lacks %q", body.MissingEvidence, missing)
		}
	}
}

func TestAdmissionEndpointRejectsUnknownAndInvalidDatabase(t *testing.T) {
	handler := admissionStatusHandler(admissionManager(t))
	if rec := getAdmission(t, handler, "/api/v1/admission/nope",
		"nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown database status = %d", rec.Code)
	}
	if rec := getAdmission(t, handler, "/api/v1/admission/x",
		"bad;name"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid database status = %d", rec.Code)
	}
	if rec := getAdmission(t, admissionStatusHandler(nil),
		"/api/v1/admission/primary", "primary"); rec.Code != http.StatusNotFound {
		t.Fatalf("nil manager status = %d", rec.Code)
	}
}

func TestAdmissionEndpointStoppedInstanceIsUnavailable(t *testing.T) {
	rec := getAdmission(t, admissionStatusHandler(admissionManager(t)),
		"/api/v1/admission/stopped", "stopped")
	var body executor.IndexAdmissionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("status %d decode %v", rec.Code, err)
	}
	if body.OK || body.Reason != "executor_unavailable" || body.Database != "stopped" {
		t.Fatalf("stopped instance admission = %+v", body)
	}
}

func TestAdmissionListEndpointSortsAndFilters(t *testing.T) {
	mgr := admissionManager(t)
	var all struct {
		Databases []executor.IndexAdmissionStatus `json:"databases"`
	}
	rec := getAdmission(t, admissionListHandler(mgr), "/api/v1/admission", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil || rec.Code != 200 {
		t.Fatalf("list status %d decode %v", rec.Code, err)
	}
	names := make([]string, 0, len(all.Databases))
	for _, db := range all.Databases {
		names = append(names, db.Database)
	}
	if !slices.Equal(names, []string{"analytics", "primary", "stopped"}) {
		t.Fatalf("databases = %v", names)
	}
	rec = getAdmission(t, admissionListHandler(mgr), "/api/v1/admission?database=primary", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil ||
		len(all.Databases) != 1 || all.Databases[0].Database != "primary" {
		t.Fatalf("filtered list = %+v %v", all.Databases, err)
	}
	if rec := getAdmission(t, admissionListHandler(mgr),
		"/api/v1/admission?database=missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown filter status = %d", rec.Code)
	}
	if rec := getAdmission(t, admissionListHandler(nil),
		"/api/v1/admission", ""); rec.Code != http.StatusOK {
		t.Fatalf("nil manager list status = %d", rec.Code)
	}
}
