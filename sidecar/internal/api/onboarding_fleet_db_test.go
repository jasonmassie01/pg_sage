package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// deadPool is a pool whose server refuses every connection.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage:pw@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func fleetReader(t *testing.T, insts ...*fleet.DatabaseInstance) onboardingReader {
	t.Helper()
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	for _, inst := range insts {
		mgr.RegisterInstance(inst)
	}
	return newFleetOnboardingReader(mgr, cfg, nil, nil)
}

// TestFleetOnboardingLiveAndDownDatabase: in a two-database fleet whose
// second server refuses connections, the live database's checklist comes
// from its stored first look and the other says it cannot be reached.
func TestFleetOnboardingLiveAndDownDatabase(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "api_fl_fleet")
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
	now := time.Now()
	if _, err := firstlook.NewStore(live).Save(ctx, firstlook.Report{Database: "live",
		StartedAt: now, FinishedAt: now, DurationMS: 40,
		Items: []firstlook.Item{{Rule: firstlook.RuleDuplicateIndex, Object: "public.t"}},
		Checks: []firstlook.Check{{Rule: firstlook.RuleSequenceRunway,
			Status: firstlook.CheckDegraded}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	reader := fleetReader(t,
		&fleet.DatabaseInstance{Name: "live", Pool: live,
			Status: &fleet.InstanceStatus{Connected: true, TrustLevel: "observation"}},
		&fleet.DatabaseInstance{Name: "down", Pool: deadPool(t),
			Status: &fleet.InstanceStatus{Connected: true, TrustLevel: "observation"}})
	rec, body := obGet(t, onboardingHandler(reader), "/api/v1/onboarding")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	up := entryNamed(t, body, "live")
	fl := up["first_look"].(map[string]any)
	if _, has := up["error"]; has || fl["items"] != float64(1) ||
		fl["degraded"] != float64(1) || fl["checks"] != float64(1) {
		t.Fatalf("live entry = %v", up)
	}
	down := entryNamed(t, body, "down")
	if down["error"] != "unreachable" || down["connected"] != false {
		t.Fatalf("down entry = %v", down)
	}
	_, body = obGet(t, firstLookHandler(reader), "/api/v1/first-look")
	if entryNamed(t, body, "live")["report"] == nil ||
		entryNamed(t, body, "down")["error"] != "unreachable" {
		t.Fatalf("first look = %v", body)
	}
}
