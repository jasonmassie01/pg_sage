package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-07: without the alerting owner registered, turning alerting off
// from Settings is persisted and reported pending restart. (alerting-06b
// gave alerting.enabled the alerting owner, which reconciles it live; the
// metadata check below used to pin restart with no owner.)
func TestGlobalPutAlertingEnabledIsPendingRestart(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetWave2GlobalConfig(t, ctx, pool)
	clear := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND key = 'alerting.enabled'`)
	}
	clear()
	t.Cleanup(clear)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	base.Alerting.Enabled = true
	controller := config.NewConfigController(base, nil, apiTrustOwner{},
		namedAPIOwner(config.LLMOwner), namedAPIOwner(config.FleetDatabasesOwner))
	put := configGlobalPutHandler(cs, controller)
	response := doRequestWithUser(put, http.MethodPut, "/api/v1/config/global",
		`{"alerting.enabled":"false","expected_generation":1}`, user)
	if response.Code != http.StatusOK {
		t.Fatalf("put status = %d; body=%s", response.Code, response.Body.String())
	}
	var body config.ApplyResult
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.PendingRestart) != 1 || body.PendingRestart[0] != "alerting.enabled" ||
		len(body.Applied) != 0 {
		t.Fatalf("applied/pending = %v/%v, want pending [alerting.enabled]",
			body.Applied, body.PendingRestart)
	}
	if !controller.Active().Config.Alerting.Enabled ||
		controller.Desired().Config.Alerting.Enabled {
		t.Fatal("active must keep alerting on until restart; desired must turn it off")
	}
	meta, ok := config.LookupFieldLifecycle("alerting.enabled")
	if !ok || meta.Lifecycle != config.LifecycleReconfigure ||
		meta.Owner != config.AlertingOwner {
		t.Fatalf("alerting.enabled metadata = %+v, want reconfigure by alerting", meta)
	}
	if got := controller.PendingRestart(); len(got) != 1 || got[0] != "alerting.enabled" {
		t.Fatalf("controller pending restart = %v", got)
	}
}

type namedAPIOwner string

func (o namedAPIOwner) Name() string { return string(o) }

func (namedAPIOwner) Prepare(
	context.Context, config.ConfigSnapshot, config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	return apiPreparedConfig{}, nil
}
