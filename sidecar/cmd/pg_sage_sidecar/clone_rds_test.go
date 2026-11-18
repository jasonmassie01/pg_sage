package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// lab-05d: clone.provider rds_restore is wired into the migration factory
// and the reaper; a rehearsal on it is bounded by clone.restore_deadline
// plus the rehearsal itself. No test reaches AWS.

func rdsRestoreCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "rds_restore", MaxCloneAgeMinutes: 60,
		AllowFullCopy: true, RestoreDeadlineMinutes: 120,
		RDS: config.CloneRDSConfig{Region: "us-east-1"}}
}

func TestConfiguredCloneProviderBuildsRDSRestore(t *testing.T) {
	p, err := configuredMCPCloneProvider(rdsRestoreCfg())
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := p.(*clone.SnapshotProvider)
	if !ok {
		t.Fatalf("rds_restore built %T", p)
	}
	c, err := sp.Describe(context.Background())
	if err != nil || c.Kind != clone.KindFullCopy || c.MaxTTL != 3*time.Hour+15*time.Minute {
		t.Fatalf("rds_restore capability = %+v, %v", c, err)
	}
	if _, ok := p.(clone.Finder); !ok {
		t.Fatal("rds_restore cannot find an uncertain restore by name")
	}
	bad := rdsRestoreCfg()
	bad.RDS.Region = "nowhere"
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid rds block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfRDSRestoreIsTheRegion(t *testing.T) {
	if got := cloneReceiptScope(rdsRestoreCfg()); got != "aws/us-east-1" {
		t.Fatalf("rds_restore receipt scope = %q, want aws/us-east-1", got)
	}
}

// Boundary: the rehearsal bound covers the restore deadline and then the
// rehearsal; the receipt outlives both by the usual margin. Other
// providers keep theirs.
func TestRDSRestoreRehearsalBoundAndReceiptTTL(t *testing.T) {
	for _, tc := range []struct{ deadline, bound int }{{30, 90}, {120, 180}, {720, 780}} {
		c := rdsRestoreCfg()
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
	for _, c := range []config.CloneProviderConfig{auroraCloneCfg(), {Provider: "dle"}} {
		if got := rehearsalReceiptTTL(c); got != rehearsal.DefaultMaxDuration+15*time.Minute {
			t.Errorf("%s receipt TTL = %s, want 1h15m", c.Provider, got)
		}
	}
}

// The reaper's adapter has no production pool: a create is refused before
// AWS is contacted (the credentials here are a fake that fails the test if
// retrieved).
func TestRDSRestoreWithoutProductionNeverCreates(t *testing.T) {
	retrieved := false
	p, err := clone.NewRDSRestoreProvider(clone.RDSRestoreConfig{Region: "us-east-1",
		Deadline: 2 * time.Hour, TTL: 3 * time.Hour,
		API: rdsRestoreAPI("us-east-1", func(_ context.Context, region string) (aws.Config,
			error) {
			retrieved = true
			return aws.Config{Region: region, Credentials: staticCreds{}}, nil
		}), Production: rdsProduction(nil)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(),
		clone.CloneSpec{ResourceName: "sage-rehearsal-0a1b2c3d"})
	if !errors.Is(err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(),
		"production") {
		t.Fatalf("create without a production pool = %v", err)
	}
	if retrieved {
		t.Fatal("the AWS configuration was loaded for a create that cannot happen")
	}
}

func TestRDSRestoreAPIChecksCredentialsFirst(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(context.Context, string) (aws.Config, error){
		"load fails": func(context.Context, string) (aws.Config, error) {
			return aws.Config{}, errors.New("bad shared config")
		},
		"no provider": fakeAWSConfig(nil),
		"retrieve fails": fakeAWSConfig(staticCreds{
			err: errors.New("no EC2 IMDS role found")}),
	}
	for name, load := range cases {
		if api, err := rdsRestoreAPI("us-east-1", load)(ctx); err == nil || api != nil {
			t.Errorf("%s: api = %v, err = %v; want an error", name, api, err)
		}
	}
	api, err := rdsRestoreAPI("us-east-1", fakeAWSConfig(staticCreds{}))(ctx)
	if err != nil || api == nil {
		t.Fatalf("with credentials: %v, %v", api, err)
	}
}
