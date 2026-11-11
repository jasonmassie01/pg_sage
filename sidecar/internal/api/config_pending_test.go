package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

type pendingGetBody struct {
	RestartSupported *bool          `json:"restart_supported"`
	PendingRestart   []string       `json:"pending_restart"`
	Pending          []pendingEntry `json:"pending"`
}

// pendingController returns a controller whose desired config moved
// trust.maintenance_window (restart-bound without the trust owner) and
// llm.api_key (restart-bound without the LLM owner) away from the active one.
func pendingController(t *testing.T) *config.ConfigController {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.MaintenanceWindow = "weekends"
	controller := config.NewConfigController(cfg, nil)
	candidate := controller.Desired().Config
	candidate.Trust.MaintenanceWindow = "weeknights America/Chicago"
	candidate.LLM.APIKey = "sk-pending-secret-value"
	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply candidate: %v", err)
	}
	if len(result.PendingRestart) != 2 {
		t.Fatalf("pending_restart = %v, want the two restart-bound keys",
			result.PendingRestart)
	}
	return controller
}

func decodePendingGet(t *testing.T, handler http.HandlerFunc) (pendingGetBody, string) {
	t.Helper()
	w := doRequest(handler, http.MethodGet, "/api/v1/config/global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	var body pendingGetBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body, raw
}

func pendingByKey(entries []pendingEntry) map[string]pendingEntry {
	byKey := make(map[string]pendingEntry, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = entry
	}
	return byKey
}

func TestConfigGlobalGet_PendingCarriesActiveAndDesired(t *testing.T) {
	controller := pendingController(t)
	body, _ := decodePendingGet(t, configReadOnlyGlobalGetHandler(controller))
	window, ok := pendingByKey(body.Pending)["trust.maintenance_window"]
	if !ok {
		t.Fatalf("pending = %+v, want trust.maintenance_window", body.Pending)
	}
	if window.Active != "weekends" || window.Desired != "weeknights America/Chicago" {
		t.Errorf("window active=%v desired=%v, want weekends -> weeknights",
			window.Active, window.Desired)
	}
	if window.Lifecycle != string(config.LifecycleRestart) || window.Hidden {
		t.Errorf("window lifecycle=%q hidden=%v, want restart and shown",
			window.Lifecycle, window.Hidden)
	}
	if len(body.Pending) != len(body.PendingRestart) {
		t.Errorf("pending has %d entries, pending_restart %d keys",
			len(body.Pending), len(body.PendingRestart))
	}
}

// A secret pending value is listed by key only; neither value is sent.
func TestConfigGlobalGet_PendingHidesSecretValues(t *testing.T) {
	controller := pendingController(t)
	body, raw := decodePendingGet(t, configReadOnlyGlobalGetHandler(controller))
	key, ok := pendingByKey(body.Pending)["llm.api_key"]
	if !ok {
		t.Fatalf("pending = %+v, want llm.api_key", body.Pending)
	}
	if !key.Hidden || key.Active != nil || key.Desired != nil {
		t.Errorf("llm.api_key entry = %+v, want hidden with no values", key)
	}
	if strings.Contains(raw, "sk-pending-secret-value") {
		t.Fatal("GET /config/global leaked the pending secret value")
	}
}

// State transitions: restart_supported follows SetRestartFunc.
func TestConfigGlobalGet_RestartSupportedFollowsSupervisor(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	handler := configReadOnlyGlobalGetHandler(controller)
	SetRestartFunc(nil)
	body, _ := decodePendingGet(t, handler)
	if body.RestartSupported == nil || *body.RestartSupported {
		t.Fatalf("restart_supported = %v, want false without a supervisor",
			body.RestartSupported)
	}
	SetRestartFunc(func() {})
	defer SetRestartFunc(nil)
	body, _ = decodePendingGet(t, handler)
	if body.RestartSupported == nil || !*body.RestartSupported {
		t.Fatalf("restart_supported = %v, want true under a supervisor",
			body.RestartSupported)
	}
}

// Nil/empty: nothing pending is an empty array, never null.
func TestConfigGlobalGet_NothingPendingIsEmptyArray(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	_, raw := decodePendingGet(t, configReadOnlyGlobalGetHandler(controller))
	if !strings.Contains(raw, `"pending":[]`) {
		t.Fatalf("body %s, want \"pending\":[]", raw)
	}
	if entries := pendingEntries(nil); entries == nil || len(entries) != 0 {
		t.Fatalf("pendingEntries(nil) = %#v, want an empty slice", entries)
	}
}

// Lifecycle: a key whose owner is registered applies live, so it is
// never listed as pending.
func TestConfigGlobalGet_LiveOwnerKeyIsNotPending(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	if lifecycle, _ := controller.EffectiveLifecycle("trust.maintenance_window"); lifecycle !=
		config.LifecycleRestart {
		t.Fatalf("lifecycle without owner = %q, want restart", lifecycle)
	}
	entries := pendingEntries(controller)
	if len(entries) != 0 {
		t.Fatalf("pending without changes = %+v, want none", entries)
	}
}

// Concurrent: GETs racing an Apply each see a whole snapshot.
func TestConfigGlobalGet_PendingUnderConcurrentApply(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	handler := configReadOnlyGlobalGetHandler(controller)
	var wg sync.WaitGroup
	failures := make(chan string, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				w := doRequest(handler, http.MethodGet, "/api/v1/config/global", "")
				var body pendingGetBody
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil ||
					w.Code != http.StatusOK {
					failures <- "status " + w.Body.String()
					continue
				}
				for _, entry := range body.Pending {
					if entry.Key == "" || entry.Lifecycle == "" {
						failures <- entry.Key
					}
				}
			}
		}()
	}
	candidate := controller.Desired().Config
	candidate.Trust.MaintenanceWindow = "nights"
	if _, err := controller.Apply(context.Background(), 1, candidate); err != nil {
		t.Errorf("apply: %v", err)
	}
	wg.Wait()
	close(failures)
	for key := range failures {
		t.Errorf("incomplete pending entry for %q", key)
	}
	if got := pendingByKey(pendingEntries(controller)); got["trust.maintenance_window"].Desired !=
		"nights" {
		t.Errorf("after apply pending = %+v, want the window desired nights", got)
	}
}

// Integration: the persistent GET (control database) reports the same.
func TestConfigGlobalGet_PersistentHandlerReportsPending(t *testing.T) {
	cs, _ := contractSetup(t)
	SetRestartFunc(nil)
	controller := pendingController(t)
	body, _ := decodePendingGet(t, configGlobalGetHandler(cs, controller))
	if body.RestartSupported == nil || *body.RestartSupported {
		t.Fatalf("restart_supported = %v, want false", body.RestartSupported)
	}
	window := pendingByKey(body.Pending)["trust.maintenance_window"]
	if window.Active != "weekends" || window.Desired != "weeknights America/Chicago" {
		t.Fatalf("pending = %+v, want the window weekends -> weeknights", body.Pending)
	}
}
