package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// Fast elevation is never silent: the autonomy view says whether any
// elevation setting is below the spec and lists each lowered value, and
// says "inactive" with an empty list otherwise.

func (f *autonomyAPIFixture) routerWith(user *auth.User,
	lowered []config.LoweredSetting) http.Handler {
	f.t.Helper()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(f.mgr, config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{Autonomy: &AutonomyDeps{Ledgers: f.reg, FastElevation: lowered}}, inject)
}

func fastElevationOf(t *testing.T, body map[string]any) (bool, []any) {
	t.Helper()
	fe, ok := body["fast_elevation"].(map[string]any)
	if !ok {
		t.Fatalf("view has no fast_elevation object: %v", body)
	}
	active, ok := fe["active"].(bool)
	if !ok {
		t.Fatalf("fast_elevation.active = %v", fe["active"])
	}
	lowered, ok := fe["lowered"].([]any)
	if !ok {
		t.Fatalf("fast_elevation.lowered = %#v, want a list", fe["lowered"])
	}
	return active, lowered
}

func TestAutonomyAPI_FastElevationInactiveAtSpecDefaults(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	for _, lowered := range [][]config.LoweredSetting{nil, {}} {
		code, body := autonomyCall(t, f.routerWith(testViewerUser(), lowered), "GET",
			"/api/v1/sre/autonomy?database=orders", "")
		active, list := fastElevationOf(t, body)
		if code != 200 || active || len(list) != 0 {
			t.Fatalf("spec defaults: %d active=%v lowered=%v", code, active, list)
		}
	}
}

func TestAutonomyAPI_FastElevationListsLoweredValues(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	lowered := []config.LoweredSetting{
		{Key: "trust.ramp_safe_hours", Value: 1, Default: 192, Unit: "hours"},
		{Key: "sre.autonomy.promotion.min_live_recoveries", Value: 3, Default: 50,
			Unit: "recoveries"},
	}
	code, body := autonomyCall(t, f.routerWith(testViewerUser(), lowered), "GET",
		"/api/v1/sre/autonomy?database=orders", "")
	active, list := fastElevationOf(t, body)
	if code != 200 || !active || len(list) != 2 {
		t.Fatalf("fast elevation: %d active=%v lowered=%v", code, active, list)
	}
	first := list[0].(map[string]any)
	if first["key"] != "trust.ramp_safe_hours" || first["value"] != 1.0 ||
		first["default"] != 192.0 || first["unit"] != "hours" {
		t.Fatalf("first lowered = %v", first)
	}
	if body["enforced"] != true {
		t.Fatalf("fast elevation changed enforcement: %v", body["enforced"])
	}
}
