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

// lab-05e: clone.provider cloudsql_clone is wired into the migration
// factory and the reaper; a rehearsal on it is bounded by
// clone.restore_deadline plus the rehearsal itself. No test reaches
// Google or reads this machine's credentials.

func cloudSQLCloneCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "cloudsql_clone", MaxCloneAgeMinutes: 60,
		AllowFullCopy: true, RestoreDeadlineMinutes: 120,
		CloudSQL: config.CloneCloudSQLConfig{Project: "acme-prod-1234", Instance: "shop"}}
}

func TestConfiguredCloneProviderBuildsCloudSQLClone(t *testing.T) {
	p, err := configuredMCPCloneProvider(cloudSQLCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := p.(*clone.SnapshotProvider)
	if !ok {
		t.Fatalf("cloudsql_clone built %T", p)
	}
	c, err := sp.Describe(context.Background())
	if err != nil || c.Kind != clone.KindFullCopy || c.MaxTTL != 3*time.Hour+15*time.Minute {
		t.Fatalf("cloudsql_clone capability = %+v, %v", c, err)
	}
	if _, ok := p.(clone.Finder); !ok {
		t.Fatal("cloudsql_clone cannot find an uncertain clone by name")
	}
	bad := cloudSQLCloneCfg()
	bad.CloudSQL.Project = "Not A Project"
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid cloudsql block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfCloudSQLCloneIsTheProject(t *testing.T) {
	if got := cloneReceiptScope(cloudSQLCloneCfg()); got != "gcp/acme-prod-1234" {
		t.Fatalf("cloudsql_clone receipt scope = %q, want gcp/acme-prod-1234", got)
	}
}

// Boundary: the rehearsal bound covers the restore deadline and then the
// rehearsal; the receipt outlives both by the usual margin.
func TestCloudSQLCloneRehearsalBoundAndReceiptTTL(t *testing.T) {
	for _, tc := range []struct{ deadline, bound int }{{30, 90}, {120, 180}, {720, 780}} {
		c := cloudSQLCloneCfg()
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
func TestCloudSQLCloneWithoutProductionNeverCreates(t *testing.T) {
	loaded := false
	p, err := clone.NewCloudSQLCloneProvider(clone.CloudSQLCloneConfig{
		Project: "acme-prod-1234", Instance: "shop", Deadline: 2 * time.Hour,
		TTL: 3 * time.Hour, Production: cloudSQLProduction(nil),
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

// Game days never make a full copy: with cloudsql_clone and no local
// database they stay off.
func TestGameDayProviderNeverPicksCloudSQLClone(t *testing.T) {
	p, name, err := gameDayProvider(config.SREGameDaysConfig{Enabled: true},
		cloudSQLCloneCfg(), localTargetGuard{})
	if err != nil || p != nil || name == clone.CloudSQLCloneAdapter {
		t.Fatalf("game day provider = %v, %q, %v; want none", p, name, err)
	}
}

func TestCloudSQLProductionIsNilWithoutAPool(t *testing.T) {
	if cloudSQLProduction(nil) != nil {
		t.Fatal("a nil pool has a production identity")
	}
}
