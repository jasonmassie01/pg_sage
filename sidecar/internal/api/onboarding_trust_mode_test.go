package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// ux-04b: the setup wizard's autonomy step writes trust.level and, where
// the config API can store it, execution_mode (decision D6). The trust
// guide says which, and the database's current execution mode.
// No concurrent access test: target is a pure read of the config mode.

func TestTrustTargetSaysWhereExecutionModeIsWritable(t *testing.T) {
	inst := &fleet.DatabaseInstance{Name: "app", DatabaseID: 3}
	for name, tc := range map[string]struct {
		cfg      config.Config
		inst     *fleet.DatabaseInstance
		url      string
		method   string
		writable bool
	}{
		"meta-db row": {config.Config{MetaDB: "postgres://m"}, inst,
			"/api/v1/config/databases/3", "api", true},
		"meta-db no row": {config.Config{MetaDB: "postgres://m"},
			&fleet.DatabaseInstance{Name: "x"}, "/api/v1/config/global", "api", false},
		"standalone global": {config.Config{}, inst, "/api/v1/config/global", "api", false},
		"yaml fleet":        {config.Config{Mode: "fleet"}, inst, "", "yaml", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fleetOnboardingReader{cfg: &tc.cfg}
			got := f.target(tc.inst)
			if got.Method != tc.method || got.ConfigURL != tc.url ||
				got.ExecutionModeWritable != tc.writable || got.Key != "trust.level" {
				t.Fatalf("target = %+v, want %s %q writable %v", got, tc.method, tc.url,
					tc.writable)
			}
		})
	}
}

func TestTrustGuideReportsTheExecutionMode(t *testing.T) {
	f := newFakeOnboarding()
	f.guide.ExecutionMode = "approval"
	f.target.ExecutionModeWritable = true
	rec, body := obGet(t, trustGuideHandler(f), "/api/v1/onboarding/trust?database=app")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	grant, _ := body["grant"].(map[string]any)
	if body["execution_mode"] != "approval" || grant["execution_mode_writable"] != true {
		t.Fatalf("body = %v, want execution_mode approval and a writable grant", body)
	}
	f.guide.ExecutionMode, f.target.ExecutionModeWritable = "", false
	_, body = obGet(t, trustGuideHandler(f), "/api/v1/onboarding/trust?database=app")
	if grant, _ := body["grant"].(map[string]any); body["execution_mode"] != "" ||
		grant["execution_mode_writable"] != false {
		t.Fatalf("unknown mode body = %v, want an empty mode, not writable", body)
	}
}
