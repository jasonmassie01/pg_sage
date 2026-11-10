package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-03 on real PostgreSQL: a trust raise saved through either write
// endpoint over an explicit file observation is stored but held back, and
// reported as masked, instead of raising trust until the next reload.

// maskingPutController publishes the YAML observation the way startup
// does: the file layer replayed with the stored overrides (none yet), so
// the desired config records its file base.
func maskingPutController(t *testing.T) *config.ConfigController {
	t.Helper()
	base := loadMaskingBase(t, "trust:\n  level: observation\n")
	candidate := config.Clone(base)
	if masked := ReplayStoredOverrides(candidate, nil, nil); len(masked) != 0 {
		t.Fatalf("replay with no overrides masked %+v", masked)
	}
	return config.NewConfigController(candidate, nil)
}

func assertTrustRaiseHeldBack(
	t *testing.T, controller *config.ConfigController, cs *store.ConfigStore,
	response int, body string,
) {
	t.Helper()
	if response != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", response, body)
	}
	if got := controller.Desired().Config.Trust.Level; got != "observation" {
		t.Fatalf("desired trust.level = %q, want the YAML observation", got)
	}
	masked := controller.MaskedByOverride()
	if len(masked) != 1 || masked[0].Key != "trust.level" ||
		masked[0].OverrideValue != "autonomous" ||
		masked[0].EffectiveValue != "observation" ||
		masked[0].FileValue != "observation" {
		t.Fatalf("MaskedByOverride = %+v, want trust.level held at observation", masked)
	}
	overrides, err := cs.GetOverrides(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range overrides {
		if o.Key == "trust.level" && o.Value == "autonomous" {
			return
		}
	}
	t.Fatalf("stored overrides = %+v, want trust.level=autonomous kept", overrides)
}

func TestGlobalPutTrustRaiseOverExplicitFileIsHeldBack(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetMaskingGlobalConfig(t, ctx, pool)
	cs, user := store.NewConfigStore(pool), maskingAdmin(t)
	controller := maskingPutController(t)
	w := doRequestWithUserRetry(t, configGlobalPutHandler(cs, controller),
		http.MethodPut, "/api/v1/config/global",
		`{"expected_generation": 1, "trust.level": "autonomous"}`, user)
	assertTrustRaiseHeldBack(t, controller, cs, w.Code, w.Body.String())
}

func TestConfigPutTrustRaiseOverExplicitFileIsHeldBack(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetMaskingGlobalConfig(t, ctx, pool)
	cs, user := store.NewConfigStore(pool), maskingAdmin(t)
	controller := maskingPutController(t)
	w := doRequestWithUserRetry(t, configUpdateHandlerWithStore(controller, cs),
		http.MethodPut, "/api/v1/config",
		`{"expected_generation": 1, "trust": {"level": "autonomous"}}`, user)
	assertTrustRaiseHeldBack(t, controller, cs, w.Code, w.Body.String())
}

func maskingAdmin(t *testing.T) *auth.User {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	return &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
}
