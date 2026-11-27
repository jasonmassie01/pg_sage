package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// lab-05f: clone.provider alloydb_restore is wired into the migration
// factory and the reaper; a rehearsal on it is bounded by
// clone.restore_deadline plus the rehearsal itself. No test reaches
// Google or reads this machine's credentials.

func alloyDBRestoreCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "alloydb_restore", MaxCloneAgeMinutes: 60,
		AllowFullCopy: true, RestoreDeadlineMinutes: 120,
		AlloyDB: config.CloneAlloyDBConfig{Project: "acme-prod-1234",
			Location: "us-central1", Cluster: "shop"}}
}

func TestConfiguredCloneProviderBuildsAlloyDBRestore(t *testing.T) {
	p, err := configuredMCPCloneProvider(alloyDBRestoreCfg())
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := p.(*clone.SnapshotProvider)
	if !ok {
		t.Fatalf("alloydb_restore built %T", p)
	}
	c, err := sp.Describe(context.Background())
	if err != nil || c.Kind != clone.KindFullCopy || c.MaxTTL != 3*time.Hour+15*time.Minute {
		t.Fatalf("alloydb_restore capability = %+v, %v", c, err)
	}
	if _, ok := p.(clone.Finder); !ok {
		t.Fatal("alloydb_restore cannot find an uncertain clone by name")
	}
	bad := alloyDBRestoreCfg()
	bad.AlloyDB.Location = "us-central1-a"
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid alloydb block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfAlloyDBRestoreIsTheProjectAndRegion(t *testing.T) {
	want := "gcp/acme-prod-1234/us-central1"
	if got := cloneReceiptScope(alloyDBRestoreCfg()); got != want {
		t.Fatalf("alloydb_restore receipt scope = %q, want %s", got, want)
	}
}

// Boundary: the rehearsal bound covers the restore deadline and then the
// rehearsal; the receipt outlives both by the usual margin.
func TestAlloyDBRestoreRehearsalBoundAndReceiptTTL(t *testing.T) {
	for _, tc := range []struct{ deadline, bound int }{{30, 90}, {120, 180}, {720, 780}} {
		c := alloyDBRestoreCfg()
		c.RestoreDeadlineMinutes = tc.deadline
		want := time.Duration(tc.bound) * time.Minute
		if got := rehearsalMaxDuration(c); got != want {
			t.Errorf("deadline %d: bound %s, want %s", tc.deadline, got, want)
		}
		if got := rehearsalReceiptTTL(c); got != want+15*time.Minute {
			t.Errorf("deadline %d: receipt TTL %s, want %s", tc.deadline, got,
				want+15*time.Minute)
		}
	}
}

// The reaper's adapter has no production pool: a create is refused before
// the Google credential chain is loaded.
func TestAlloyDBRestoreWithoutProductionNeverCreates(t *testing.T) {
	loaded := false
	p, err := clone.NewAlloyDBRestoreProvider(clone.AlloyDBRestoreConfig{
		Project: "acme-prod-1234", Location: "us-central1", Cluster: "shop",
		Deadline: 2 * time.Hour, TTL: 3 * time.Hour, Production: alloyDBProduction(nil),
		Token: func(context.Context) (clone.CloudSQLTokenSource, error) {
			loaded = true
			return nil, errors.New("must not be loaded")
		}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(),
		clone.CloneSpec{ResourceName: "sage-rehearsal-0a1b2c3d"})
	if !errors.Is(err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(),
		"production") || loaded {
		t.Fatalf("create without a production pool = %v (credentials loaded %v)", err, loaded)
	}
}

// Game days never make a full copy: with alloydb_restore and no local
// database they stay off.
func TestGameDayProviderNeverPicksAlloyDBRestore(t *testing.T) {
	p, name, err := gameDayProvider(config.SREGameDaysConfig{Enabled: true},
		alloyDBRestoreCfg(), localTargetGuard{})
	if err != nil || p != nil || name == clone.AlloyDBRestoreAdapter {
		t.Fatalf("game day provider = %v, %q, %v; want none", p, name, err)
	}
}

func TestAlloyDBProductionIsNilWithoutAPool(t *testing.T) {
	if alloyDBProduction(nil) != nil {
		t.Fatal("a nil pool has a production identity")
	}
}
