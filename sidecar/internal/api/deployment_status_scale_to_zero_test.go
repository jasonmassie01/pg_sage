package api

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

const scaleEndpoint = "projects/p/branches/b/endpoints/primary"

func scaleStatusRow(t *testing.T, info *databricksauth.EndpointInfo) (map[string]any, string) {
	t.Helper()
	rt := RuntimeDeps{
		LakebaseCredentials: func() []databricksauth.EndpointCredential {
			return []databricksauth.EndpointCredential{{Endpoint: scaleEndpoint}}
		},
		LakebaseEndpointInfo: func(endpoint string) (databricksauth.EndpointInfo, bool) {
			if info == nil || endpoint != scaleEndpoint {
				return databricksauth.EndpointInfo{}, false
			}
			return *info, true
		},
	}
	result, body := deploymentStatus(t, config.DefaultConfig(), rt)
	return databricksBlock(t, result)["endpoints"].([]any)[0].(map[string]any), body
}

// Happy path: a suspend timeout with blockers reports keeps_compute_active.
func TestDeploymentStatusReportsScaleToZeroBlockers(t *testing.T) {
	observed := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	row, _ := scaleStatusRow(t, &databricksauth.EndpointInfo{
		SuspendTimeout: 5 * time.Minute, State: "ACTIVE", ObservedAt: observed,
		Blockers: []string{"collector every 1m0s", "health check every 30s"},
	})
	if row["suspend_timeout_seconds"] != float64(300) || row["keeps_compute_active"] != true ||
		row["compute_state"] != "ACTIVE" ||
		row["compute_observed_at"] != observed.Format(time.RFC3339) {
		t.Fatalf("endpoint row = %v; want 300s, active compute, ACTIVE", row)
	}
	blockers, ok := row["scale_to_zero_blockers"].([]any)
	if !ok || len(blockers) != 2 || blockers[0] != "collector every 1m0s" ||
		blockers[1] != "health check every 30s" {
		t.Fatalf("blockers = %v", row["scale_to_zero_blockers"])
	}
}

// Nil/empty: scale-to-zero off reports null seconds, keeps_compute_active
// false and an empty (not null) blocker list.
func TestDeploymentStatusScaleToZeroOff(t *testing.T) {
	row, body := scaleStatusRow(t, &databricksauth.EndpointInfo{State: "IDLE",
		ObservedAt: time.Now()})
	if row["suspend_timeout_seconds"] != nil || row["keeps_compute_active"] != false {
		t.Fatalf("scale-to-zero off row = %v", row)
	}
	if !strings.Contains(body, `"scale_to_zero_blockers":[]`) {
		t.Fatalf("blockers not an empty array: %s", body)
	}
}

// Boundary: never observed (or no lookup wired) reports null, not false:
// pg_sage does not know whether it keeps the compute active.
func TestDeploymentStatusScaleToZeroUnobserved(t *testing.T) {
	row, body := scaleStatusRow(t, nil)
	for _, key := range []string{"suspend_timeout_seconds", "keeps_compute_active",
		"compute_state", "compute_observed_at"} {
		if v, present := row[key]; !present || v != nil {
			t.Fatalf("%s = %v (present %v); want null", key, v, present)
		}
	}
	if !strings.Contains(body, `"scale_to_zero_blockers":[]`) {
		t.Fatalf("blockers not an empty array: %s", body)
	}
	result, _ := deploymentStatus(t, config.DefaultConfig(), RuntimeDeps{
		LakebaseCredentials: func() []databricksauth.EndpointCredential {
			return []databricksauth.EndpointCredential{{Endpoint: scaleEndpoint}}
		}})
	row = databricksBlock(t, result)["endpoints"].([]any)[0].(map[string]any)
	if v, present := row["keeps_compute_active"]; !present || v != nil {
		t.Fatalf("no lookup wired: keeps_compute_active = %v (present %v)", v, present)
	}
}

// State transition: blockers noted while scale-to-zero was on are not
// reported once a later observation shows it off (suspend timeout 0).
func TestDeploymentStatusScaleToZeroTurnedOffDropsStaleBlockers(t *testing.T) {
	row, body := scaleStatusRow(t, &databricksauth.EndpointInfo{State: "ACTIVE",
		ObservedAt: time.Now(), Blockers: []string{"collector every 1m0s"}})
	if row["suspend_timeout_seconds"] != nil || row["keeps_compute_active"] != false {
		t.Fatalf("scale-to-zero off with stale blockers: row = %v", row)
	}
	if !strings.Contains(body, `"scale_to_zero_blockers":[]`) {
		t.Fatalf("stale blockers reported: %s", body)
	}
}
