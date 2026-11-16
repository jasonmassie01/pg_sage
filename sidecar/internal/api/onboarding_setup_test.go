package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// ux-04b: the checklist's Model step, the setup wizard's first-done
// times, and POST /api/v1/onboarding/setup.

var setupNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func setupPost(t *testing.T, f *fakeOnboardingReader, body string) (
	*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/setup",
		strings.NewReader(body))
	onboardingSetupHandler(f, func() time.Time { return setupNow }).ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

func entrySteps(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	entry := body["databases"].([]any)[0].(map[string]any)
	for _, s := range entry["steps"].([]any) {
		m := s.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

func TestOnboardingHandlerReportsModelStepAndSetupTimes(t *testing.T) {
	f := newFakeOnboarding()
	f.model = &onboarding.ModelStatus{Configured: true, Answered: true}
	chosen := setupNow.Add(-time.Minute)
	db := f.dbs["app"]
	db.State.AutonomyChosenAt = &chosen
	db.State.ModelConfiguredAt = &chosen
	f.dbs["app"] = db
	rec, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	steps := entrySteps(t, body)
	if m := steps[onboarding.StepModel]; m == nil || m["done"] != true {
		t.Fatalf("model step = %v, want done", m)
	}
	if g := steps[onboarding.StepGrantMore]; g["done"] != true {
		t.Fatalf("grant step after Watch only was chosen = %v, want done", g)
	}
	entry := body["databases"].([]any)[0].(map[string]any)
	setup, _ := entry["setup"].(map[string]any)
	if setup["autonomy_chosen_at"] != chosen.Format(time.RFC3339) ||
		setup["model_configured_at"] != chosen.Format(time.RFC3339) ||
		setup["channel_configured_at"] != nil {
		t.Fatalf("setup = %v, want model and autonomy times and no channel", setup)
	}
	if f.modelAsked != "app" {
		t.Fatalf("model read for %q, want the entry's database", f.modelAsked)
	}
}

func TestOnboardingHandlerModelUnknownIsNotDone(t *testing.T) {
	rec, body := obGet(t, onboardingHandler(newFakeOnboarding()), "/api/v1/onboarding")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if m := entrySteps(t, body)[onboarding.StepModel]; m["done"] != false ||
		m["optional"] != true {
		t.Fatalf("model step with no model status = %v, want optional and not done", m)
	}
}

func TestOnboardingSetupRecordsEachStep(t *testing.T) {
	for _, step := range []string{onboarding.SetupModel, onboarding.SetupChannel,
		onboarding.SetupAutonomy} {
		t.Run(step, func(t *testing.T) {
			f := newFakeOnboarding()
			f.model = &onboarding.ModelStatus{Configured: true, Answered: true}
			yes := true
			f.notify = &yes
			rec, body := setupPost(t, f, fmt.Sprintf(`{"database":"app","step":%q}`, step))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if len(f.recorded) != 1 || f.recorded[0] != "app/"+step+"@"+
				setupNow.Format(time.RFC3339) {
				t.Fatalf("recorded = %v, want app/%s at the handler's clock", f.recorded, step)
			}
			if body["recorded"] != true || body["step"] != step || body["database"] != "app" {
				t.Fatalf("body = %v", body)
			}
		})
	}
}

func TestOnboardingSetupRefusesUnfinishedSteps(t *testing.T) {
	no := false
	for name, tc := range map[string]struct {
		model  *onboarding.ModelStatus
		notify *bool
		step   string
		want   string
	}{
		"model never answered": {&onboarding.ModelStatus{Configured: true}, nil,
			onboarding.SetupModel, "has not answered"},
		"model not set":   {&onboarding.ModelStatus{}, nil, onboarding.SetupModel, "no model"},
		"model unknown":   {nil, nil, onboarding.SetupModel, "no model"},
		"no channel":      {nil, &no, onboarding.SetupChannel, "notification channel"},
		"channel unknown": {nil, nil, onboarding.SetupChannel, "notification channel"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOnboarding()
			f.model, f.notify = tc.model, tc.notify
			rec, body := setupPost(t, f, fmt.Sprintf(`{"database":"app","step":%q}`, tc.step))
			if rec.Code != http.StatusConflict ||
				!strings.Contains(fmt.Sprint(body["error"]), tc.want) {
				t.Fatalf("status %d body %v, want 409 naming %q", rec.Code, body, tc.want)
			}
			if len(f.recorded) != 0 {
				t.Fatalf("recorded %v for an unfinished step", f.recorded)
			}
		})
	}
}

func TestOnboardingSetupRefusesBadRequests(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"not json":         {`{"database":`, http.StatusBadRequest},
		"empty body":       {``, http.StatusBadRequest},
		"unknown step":     {`{"database":"app","step":"first_look"}`, http.StatusBadRequest},
		"no step":          {`{"database":"app"}`, http.StatusBadRequest},
		"no database":      {`{"step":"autonomy"}`, http.StatusBadRequest},
		"all databases":    {`{"database":"all","step":"autonomy"}`, http.StatusBadRequest},
		"bad name":         {`{"database":"bad name","step":"autonomy"}`, http.StatusBadRequest},
		"unknown database": {`{"database":"ghost","step":"autonomy"}`, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOnboarding()
			rec, _ := setupPost(t, f, tc.body)
			if rec.Code != tc.code || len(f.recorded) != 0 {
				t.Fatalf("status = %d recorded %v, want %d and nothing recorded", rec.Code,
					f.recorded, tc.code)
			}
		})
	}
	rec := httptest.NewRecorder()
	onboardingSetupHandler(nil, time.Now).ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/onboarding/setup", strings.NewReader(`{"database":"app","step":"model"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader status = %d, want 503", rec.Code)
	}
}

func TestOnboardingSetupPropagatesStoreErrors(t *testing.T) {
	f := newFakeOnboarding()
	f.recordErr = fmt.Errorf("record setup of %q: %w", "app", onboarding.ErrNotInitialized)
	rec, body := setupPost(t, f, `{"database":"app","step":"autonomy"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]),
		"not started") {
		t.Fatalf("not initialized: status %d body %v, want 409 saying it has not started",
			rec.Code, body)
	}
	f.recordErr = errors.New("connection refused")
	rec, body = setupPost(t, f, `{"database":"app","step":"autonomy"}`)
	if rec.Code != http.StatusInternalServerError ||
		strings.Contains(fmt.Sprint(body["error"]), "refused") {
		t.Fatalf("store failure: status %d body %v, want 500 without the cause", rec.Code, body)
	}
}

func TestModelStatusForPrefersTheDatabaseClient(t *testing.T) {
	ok := setupNow
	reg := fakeBudgets{status: map[string]llm.ClientStatus{
		"general":     {Enabled: true},
		"app/general": {Enabled: true, LastSuccessAt: &ok},
	}}
	if got := modelStatusFor(reg, "app"); got == nil || !got.Answered {
		t.Fatalf("app = %+v, want its own client, which answered", got)
	}
	if got := modelStatusFor(reg, "other"); got == nil || !got.Configured || got.Answered {
		t.Fatalf("other = %+v, want the process client: configured, never answered", got)
	}
	if got := modelStatusFor(nil, "app"); got == nil || got.Configured {
		t.Fatalf("no registry = %+v, want a known, unconfigured model", got)
	}
	empty := fakeBudgets{status: map[string]llm.ClientStatus{"optimizer": {Enabled: true}}}
	if got := modelStatusFor(empty, "app"); got == nil || got.Configured {
		t.Fatalf("no general client = %+v, want unconfigured", got)
	}
}

type fakeBudgets struct{ status map[string]llm.ClientStatus }

func (f fakeBudgets) TokenStatus() map[string]llm.ClientStatus { return f.status }
func (fakeBudgets) ResetBudgets()                              {}

func (f *fakeOnboardingReader) Model(_ context.Context, name string) *onboarding.ModelStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelAsked = name
	return f.model
}

func (f *fakeOnboardingReader) RecordSetup(_ context.Context, name, step string,
	at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return false, f.recordErr
	}
	f.recorded = append(f.recorded, name+"/"+step+"@"+at.Format(time.RFC3339))
	return true, nil
}
