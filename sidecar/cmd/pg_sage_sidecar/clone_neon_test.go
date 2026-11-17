package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/gameday"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// lab-05b: clone.provider neon_branch is wired into the migration
// factory, the reaper and game-day selection.

func neonCloneCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "neon_branch", MaxCloneAgeMinutes: 60,
		Neon: config.CloneNeonConfig{ProjectID: "shiny-wind-028834", TTLMinutes: 60,
			EndpointMaxCU: 1, APIKey: "napi_test_key_not_real"}}
}

func TestConfiguredCloneProviderBuildsNeonBranch(t *testing.T) {
	p, err := configuredMCPCloneProvider(neonCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	np, ok := p.(*clone.NeonBranchProvider)
	if !ok {
		t.Fatalf("neon_branch built %T", p)
	}
	if c, err := np.Describe(context.Background()); err != nil || c.Kind != clone.KindBranch ||
		c.MaxTTL.Minutes() != 60 {
		t.Fatalf("neon capability = %+v, %v", c, err)
	}
	bad := neonCloneCfg()
	bad.Neon.TTLMinutes = 0
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid neon block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfNeonIsTheProject(t *testing.T) {
	if got := cloneReceiptScope(neonCloneCfg()); got != "projects/shiny-wind-028834" {
		t.Fatalf("neon receipt scope = %q, want projects/shiny-wind-028834", got)
	}
}

// The reaper's adapter has no production pool: it can destroy, and any
// create it is asked for is refused before Neon is contacted.
func TestNeonProviderWithoutProductionNeverCreates(t *testing.T) {
	p, err := configuredMCPCloneProvider(neonCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(),
		clone.CloneSpec{ResourceName: "sage-rehearsal-0a1b2c3d"})
	if !errors.Is(err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(),
		"production") {
		t.Fatalf("create without a production pool = %v", err)
	}
}

func TestNeonProductionIsTheMonitoredDSN(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://app_owner:pw@ep-prod-100.us-east-2.aws.neon.tech:5432/appdb?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	got, err := neonProduction(pool)(context.Background())
	want := clone.NeonProduction{Host: "ep-prod-100.us-east-2.aws.neon.tech",
		User: "app_owner", Database: "appdb"}
	if err != nil || got != want {
		t.Fatalf("production = %+v, %v; want %+v", got, err, want)
	}
	if neonProduction(nil) != nil {
		t.Fatal("a nil pool produced a production source")
	}
}

// No API key makes the rehearsal recommend-only, not an error.
func TestNeonWithoutAKeyRehearsalIsRecommendOnly(t *testing.T) {
	cfg := neonCloneCfg()
	cfg.Neon.APIKey = ""
	p, err := configuredMCPCloneProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	orch := rehearsal.NewOrchestrator(p, nil, rehearsal.Options{MaxCloneAge: 60e9,
		MaxDuration: rehearsal.DefaultMaxDuration})
	result, err := recommendOnlyOnCloneFailure{orch}.Rehearse(context.Background(),
		planpkg.Plan{ExpandSteps: []planpkg.Step{{SQL: "ALTER TABLE t ADD COLUMN c int"}}})
	if err != nil || result.Verdict != rehearsal.VerdictRecommendOnly ||
		result.Reason != rehearsal.Reason("clone_unavailable") {
		t.Fatalf("keyless neon rehearsal = %+v, %v", result, err)
	}
}

func TestGameDayProviderSelectsNeonBranch(t *testing.T) {
	settings := config.DefaultConfig().SRE.Autonomy.GameDays
	settings.Enabled = true
	guard := localTargetGuard{pools: func(context.Context) ([]gameday.Querier, error) {
		return nil, nil
	}}
	p, name, err := gameDayProvider(settings, neonCloneCfg(), guard)
	if err != nil || name != "neon_branch" {
		t.Fatalf("game-day neon provider = %v %q %v", p, name, err)
	}
	if _, ok := p.(*clone.NeonBranchProvider); !ok {
		t.Fatalf("game days built %T", p)
	}
	b, bname, err := localBenchProvider(config.DefaultConfig().SRE.Autonomy,
		neonCloneCfg(), guard)
	if err != nil || bname != "neon_branch" || b == nil {
		t.Fatalf("bench neon provider = %v %q %v", b, bname, err)
	}
}
