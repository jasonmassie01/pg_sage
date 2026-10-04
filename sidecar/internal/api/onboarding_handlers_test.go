package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

type fakeOnboardingReader struct {
	names    []string
	dbs      map[string]onboardingDatabase
	dbErr    error
	mcpOn    bool
	token    *bool
	notify   *bool
	guide    onboarding.GuideInput
	target   grantTarget
	guideErr error
}

func (f *fakeOnboardingReader) Names(database string) ([]string, bool) {
	if database == "" || database == "all" {
		return f.names, true
	}
	for _, n := range f.names {
		if n == database {
			return []string{n}, true
		}
	}
	return nil, false
}

func (f *fakeOnboardingReader) Database(_ context.Context, name string) (
	onboardingDatabase, error) {
	if f.dbErr != nil {
		return onboardingDatabase{}, f.dbErr
	}
	return f.dbs[name], nil
}

func (f *fakeOnboardingReader) MCP(context.Context) (bool, *bool)   { return f.mcpOn, f.token }
func (f *fakeOnboardingReader) Notifications(context.Context) *bool { return f.notify }
func (f *fakeOnboardingReader) Guide(_ context.Context, _ string) (onboarding.GuideInput,
	grantTarget, error) {
	return f.guide, f.target, f.guideErr
}

func newFakeOnboarding() *fakeOnboardingReader {
	ttff := 9.5
	finished := time.Date(2026, 10, 4, 12, 0, 9, 0, time.UTC)
	yes := true
	return &fakeOnboardingReader{
		names: []string{"app"},
		dbs: map[string]onboardingDatabase{"app": {Name: "app", Connected: true,
			TrustLevel: "observation",
			State: &onboarding.State{Database: "app", InstallKind: onboarding.InstallNew,
				TTFFSeconds: &ttff},
			FirstLook: &firstlook.Report{Database: "app", FinishedAt: finished,
				DurationMS: 800, Items: []firstlook.Item{{Rule: firstlook.RuleDuplicateIndex,
					Severity: firstlook.SeverityWarning, Object: "public.t_b",
					Title: "Duplicate index public.t_b"}},
				Capabilities: []firstlook.Capability{{Name: firstlook.CapStatStatements,
					Status: firstlook.CapabilityOK}}}}},
		mcpOn: true, token: &yes,
		guide: onboarding.GuideInput{Current: "observation", SafeRampHours: 192,
			ModerateRampHours: 744, Tier3Safe: true},
		target: grantTarget{Method: "api", ConfigURL: "/api/v1/config/global",
			Key: "trust.level"},
	}
}

func obGet(t *testing.T, h http.Handler, target string) (*httptest.ResponseRecorder,
	map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v (%s)", target, err, rec.Body.String())
		}
	}
	return rec, body
}

func TestOnboardingHandlerReportsChecklist(t *testing.T) {
	rec, body := obGet(t, onboardingHandler(newFakeOnboarding()), "/api/v1/onboarding")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	dbs := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases = %v", dbs)
	}
	db := dbs[0].(map[string]any)
	if db["database"] != "app" || db["install_kind"] != "new" ||
		db["trust_level"] != "observation" || db["time_to_first_finding_seconds"] != 9.5 {
		t.Fatalf("database = %v", db)
	}
	fl := db["first_look"].(map[string]any)
	if fl["ready"] != true || fl["items"] != float64(1) {
		t.Fatalf("first look = %v", fl)
	}
	steps := db["steps"].([]any)
	if len(steps) != 6 {
		t.Fatalf("steps = %v", steps)
	}
	first := steps[0].(map[string]any)
	if first["id"] != onboarding.StepConnected || first["done"] != true {
		t.Fatalf("first step = %v", first)
	}
	if !strings.Contains(rec.Body.String(), `"id":"grant_more"`) {
		t.Fatalf("no grant_more step in %s", rec.Body.String())
	}
}

func TestOnboardingHandlerBeforeFirstLook(t *testing.T) {
	f := newFakeOnboarding()
	db := f.dbs["app"]
	db.FirstLook, db.State = nil, nil
	f.dbs["app"] = db
	rec, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	entry := body["databases"].([]any)[0].(map[string]any)
	if entry["first_look"].(map[string]any)["ready"] != false ||
		entry["time_to_first_finding_seconds"] != nil {
		t.Fatalf("entry before the first look = %v", entry)
	}
}

func TestOnboardingHandlerErrors(t *testing.T) {
	h := onboardingHandler(newFakeOnboarding())
	if rec, _ := obGet(t, h, "/api/v1/onboarding?database=bad%20name"); rec.Code !=
		http.StatusBadRequest {
		t.Fatalf("bad name status = %d", rec.Code)
	}
	if rec, _ := obGet(t, h, "/api/v1/onboarding?database=ghost"); rec.Code !=
		http.StatusNotFound {
		t.Fatalf("unknown database status = %d", rec.Code)
	}
	f := newFakeOnboarding()
	f.dbErr = errors.New("pq: password authentication failed for user sage")
	rec, _ := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	if rec.Code != http.StatusInternalServerError ||
		strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("reader error = %d %s, want a generic 500", rec.Code, rec.Body.String())
	}
	if rec, _ := obGet(t, onboardingHandler(nil), "/api/v1/onboarding"); rec.Code !=
		http.StatusServiceUnavailable {
		t.Fatalf("nil reader status = %d", rec.Code)
	}
}

func TestFirstLookHandler(t *testing.T) {
	rec, body := obGet(t, firstLookHandler(newFakeOnboarding()), "/api/v1/first-look?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	entry := body["databases"].([]any)[0].(map[string]any)
	report := entry["report"].(map[string]any)
	items := report["items"].([]any)
	if entry["database"] != "app" || len(items) != 1 ||
		items[0].(map[string]any)["object"] != "public.t_b" {
		t.Fatalf("first look = %v", entry)
	}
	f := newFakeOnboarding()
	db := f.dbs["app"]
	db.FirstLook = nil
	f.dbs["app"] = db
	_, body = obGet(t, firstLookHandler(f), "/api/v1/first-look")
	if body["databases"].([]any)[0].(map[string]any)["report"] != nil {
		t.Fatalf("missing report should be null: %v", body)
	}
	if rec, _ := obGet(t, firstLookHandler(f), "/api/v1/first-look?database=ghost"); rec.Code !=
		http.StatusNotFound {
		t.Fatalf("unknown database status = %d", rec.Code)
	}
}

func TestTrustGuideHandler(t *testing.T) {
	rec, body := obGet(t, trustGuideHandler(newFakeOnboarding()),
		"/api/v1/onboarding/trust?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	levels := body["levels"].([]any)
	grant := body["grant"].(map[string]any)
	if body["database"] != "app" || body["current"] != "observation" || len(levels) != 3 ||
		grant["config_url"] != "/api/v1/config/global" || grant["key"] != "trust.level" {
		t.Fatalf("trust guide = %v", body)
	}
	// With one database the parameter may be omitted.
	if rec, _ := obGet(t, trustGuideHandler(newFakeOnboarding()),
		"/api/v1/onboarding/trust"); rec.Code != http.StatusOK {
		t.Fatalf("single database without parameter = %d", rec.Code)
	}
}

func TestTrustGuideHandlerErrors(t *testing.T) {
	f := newFakeOnboarding()
	f.names = []string{"a", "b"}
	if rec, _ := obGet(t, trustGuideHandler(f), "/api/v1/onboarding/trust"); rec.Code !=
		http.StatusBadRequest {
		t.Fatalf("ambiguous database status = %d", rec.Code)
	}
	if rec, _ := obGet(t, trustGuideHandler(f), "/api/v1/onboarding/trust?database=ghost"); rec.
		Code != http.StatusNotFound {
		t.Fatalf("unknown database status = %d", rec.Code)
	}
	f = newFakeOnboarding()
	f.guideErr = errors.New("connection refused")
	if rec, _ := obGet(t, trustGuideHandler(f), "/api/v1/onboarding/trust?database=app"); rec.
		Code != http.StatusInternalServerError {
		t.Fatalf("guide error status = %d", rec.Code)
	}
}
