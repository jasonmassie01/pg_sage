package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: the handler reads one guide per request.

// Nil/empty: one identity serves one "Grant more" script and no action
// grants.
func TestTrustGuideHandler_SingleIdentityScript(t *testing.T) {
	rec, body := obGet(t, trustGuideHandler(newFakeOnboarding()),
		"/api/v1/onboarding/trust?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	scripts, ok := body["scripts"].([]any)
	if !ok || len(scripts) != 1 {
		t.Fatalf("scripts = %v, want one", body["scripts"])
	}
	if id := scripts[0].(map[string]any)["identity"]; id != onboarding.IdentitySingle {
		t.Fatalf("script identity = %v, want single", id)
	}
	if body["action_grants"] != nil {
		t.Fatalf("action_grants = %v, want null", body["action_grants"])
	}
}

// Happy path: with an action identity the guide serves both scripts and
// the action role's grants.
func TestTrustGuideHandler_SplitIdentityScripts(t *testing.T) {
	f := newFakeOnboarding()
	f.guide.ActionGrants = &onboarding.GrantStatus{Role: "sage_actor"}
	rec, body := obGet(t, trustGuideHandler(f), "/api/v1/onboarding/trust?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	scripts := body["scripts"].([]any)
	if len(scripts) != 2 {
		t.Fatalf("scripts = %v, want two", scripts)
	}
	act := scripts[1].(map[string]any)
	if act["identity"] != onboarding.IdentityAction || act["role"] != "sage_actor" {
		t.Fatalf("action script = %v", act)
	}
	if g := body["action_grants"].(map[string]any); g["role"] != "sage_actor" {
		t.Fatalf("action_grants = %v", g)
	}
}

// Integration: the fleet reader reads the action identity's grants on its
// own pool; an action pool that is down fails the guide with that named,
// rather than showing the observation role's grants as the action role's.
func TestFleetOnboardingGuide_ActionIdentityGrants(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "api_fl_action")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	live, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(live.Close)
	if err := schema.Bootstrap(ctx, live); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	status := &fleet.InstanceStatus{Connected: true, TrustLevel: "advisory"}
	split := fleetReader(t, &fleet.DatabaseInstance{Name: "live", Pool: live,
		ActionPool: live, Status: status})
	in, _, err := split.Guide(ctx, "live")
	if err != nil {
		t.Fatalf("Guide: %v", err)
	}
	if in.ActionGrants == nil || in.ActionGrants.Role != in.Grants.Role ||
		in.ActionGrants.Role == "" {
		t.Fatalf("action grants = %+v, want the action pool's role", in.ActionGrants)
	}
	single := fleetReader(t, &fleet.DatabaseInstance{Name: "live", Pool: live,
		Status: status})
	if in, _, err := single.Guide(ctx, "live"); err != nil || in.ActionGrants != nil {
		t.Fatalf("single identity Guide = (%+v, %v), want no action grants", in.ActionGrants,
			err)
	}
	down := fleetReader(t, &fleet.DatabaseInstance{Name: "live", Pool: live,
		ActionPool: deadPool(t), Status: status})
	if _, _, err := down.Guide(ctx, "live"); err == nil ||
		!strings.Contains(err.Error(), "action identity") {
		t.Fatalf("Guide with the action pool down = %v, want it named", err)
	}
}
