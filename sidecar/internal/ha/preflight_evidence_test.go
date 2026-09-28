package ha

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// No autonomous mutation is executed: this establishes monitor-to-legacy-policy admission only.
func TestPreflightEvidenceUnknownHAWithholdsPolicyAdmission(t *testing.T) {
	p, err := pgxpool.New(context.Background(), os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, warm := range []bool{false, true} {
		name := "initial_unknown"
		if warm {
			name = "lost_primary_observation"
		}
		t.Run(name, func(t *testing.T) {
			m := New(p, func(string, string, ...any) {})
			if warm && m.Check(context.Background()) {
				t.Fatal("fixture is not primary")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			replica := m.Check(ctx)
			if m.InSafeMode() {
				replica = true
			}
			cfg := config.DefaultConfig()
			cfg.Trust.Level = "autonomous"
			cfg.Trust.Tier3Safe = true
			exec := executor.New(nil, cfg, time.Now().Add(-40*24*time.Hour),
				func(string, string, ...any) {})
			exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
			exec.SetExecutionMode("auto")
			exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
			decision := exec.ExplainFamilies(context.Background(),
				[]executor.ActionContract{executor.AnalyzeTableContract()}, replica)[0]
			t.Logf("warm=%v role=%v replica=%v safe=%v policy=%s",
				warm, m.Role(), replica, m.InSafeMode(), decision.Decision)
			if decision.Decision == executor.PolicyDecisionExecute {
				t.Error("unknown role admitted a mutation at legacy policy boundary")
			}
		})
	}
}
