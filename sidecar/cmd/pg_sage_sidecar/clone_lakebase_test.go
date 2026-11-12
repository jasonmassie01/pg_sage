package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/gameday"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// market-03: clone.provider lakebase_branch is wired into the migration
// factory, the reaper and game-day selection.

func lakebaseCloneCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "lakebase_branch", MaxCloneAgeMinutes: 60,
		Lakebase: config.CloneLakebaseConfig{Project: "shop", TTLMinutes: 60,
			EndpointMaxCU: 2}}
}

func TestConfiguredCloneProviderBuildsLakebaseBranch(t *testing.T) {
	p, err := configuredMCPCloneProvider(lakebaseCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*clone.LakebaseBranchProvider); !ok {
		t.Fatalf("lakebase_branch built %T", p)
	}
	bad := lakebaseCloneCfg()
	bad.Lakebase.TTLMinutes = 0
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid lakebase block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfLakebaseIsTheProject(t *testing.T) {
	if got := cloneReceiptScope(lakebaseCloneCfg()); got != "projects/shop" {
		t.Fatalf("lakebase receipt scope = %q, want projects/shop", got)
	}
}

// The reaper's adapter has no production pool: it can destroy, and any
// create it is asked for is refused before Databricks is contacted.
func TestLakebaseProviderWithoutProductionNeverCreates(t *testing.T) {
	p, err := configuredMCPCloneProvider(lakebaseCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(), clone.CloneSpec{})
	if !errors.Is(err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(),
		"production") {
		t.Fatalf("create without a production pool = %v", err)
	}
}

func TestLakebaseProductionNeedsALakebaseEndpoint(t *testing.T) {
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage@db.invalid:5432/app?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = lakebaseProduction(pool)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a Lakebase endpoint") {
		t.Fatalf("plain PostgreSQL production = %v", err)
	}
	if lakebaseProduction(nil) != nil {
		t.Fatal("a nil pool produced a production source")
	}
}

// No production identity (or no Databricks auth) makes the rehearsal
// recommend-only, not an error.
func TestLakebaseUnavailableRehearsalIsRecommendOnly(t *testing.T) {
	p, err := configuredMCPCloneProvider(lakebaseCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	orch := rehearsal.NewOrchestrator(p, nil, rehearsal.Options{MaxCloneAge: 60e9,
		MaxDuration: rehearsal.DefaultMaxDuration})
	result, err := recommendOnlyOnCloneFailure{orch}.Rehearse(context.Background(),
		planpkg.Plan{ExpandSteps: []planpkg.Step{{SQL: "ALTER TABLE t ADD COLUMN c int"}}})
	if err != nil || result.Verdict != rehearsal.VerdictRecommendOnly ||
		result.Reason != rehearsal.Reason("clone_unavailable") {
		t.Fatalf("unavailable lakebase rehearsal = %+v, %v", result, err)
	}
}

func TestRehearsalFailureReasonOfLakebaseRefusals(t *testing.T) {
	prod := fmt.Errorf("lakebase_branch: %w", clone.ErrCloneIsProduction)
	if got := rehearsalFailureReason(prod); got != rehearsal.ReasonTargetIsProduction {
		t.Fatalf("production branch reason = %q", got)
	}
	gone := fmt.Errorf("lakebase_branch: %w", clone.ErrCloneUnavailable)
	if got := rehearsalFailureReason(gone); got != rehearsal.Reason("clone_unavailable") {
		t.Fatalf("unavailable reason = %q", got)
	}
}

func TestGameDayProviderSelectsLakebaseBranch(t *testing.T) {
	settings := config.DefaultConfig().SRE.Autonomy.GameDays
	settings.Enabled = true
	guard := localTargetGuard{pools: func(context.Context) ([]gameday.Querier, error) {
		return nil, nil
	}}
	p, name, err := gameDayProvider(settings, lakebaseCloneCfg(), guard)
	if err != nil || name != "lakebase_branch" {
		t.Fatalf("game-day lakebase provider = %v %q %v", p, name, err)
	}
	if _, ok := p.(*clone.LakebaseBranchProvider); !ok {
		t.Fatalf("game days built %T", p)
	}
	b, bname, err := localBenchProvider(config.DefaultConfig().SRE.Autonomy,
		lakebaseCloneCfg(), guard)
	if err != nil || bname != "lakebase_branch" || b == nil {
		t.Fatalf("bench lakebase provider = %v %q %v", b, bname, err)
	}
}
