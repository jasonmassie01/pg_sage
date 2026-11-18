package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/gameday"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

// lab-05c: clone.provider aurora_clone is wired into the migration
// factory, the reaper and game-day selection. No test reaches AWS.

func auroraCloneCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "aurora_clone", MaxCloneAgeMinutes: 60,
		Aurora: config.CloneAuroraConfig{Region: "us-east-1", TTLMinutes: 60}}
}

func TestConfiguredCloneProviderBuildsAuroraClone(t *testing.T) {
	p, err := configuredMCPCloneProvider(auroraCloneCfg())
	if err != nil {
		t.Fatal(err)
	}
	ap, ok := p.(*clone.AuroraCloneProvider)
	if !ok {
		t.Fatalf("aurora_clone built %T", p)
	}
	if c, err := ap.Describe(context.Background()); err != nil ||
		c.Kind != clone.KindCopyOnWrite || c.MaxTTL != time.Hour {
		t.Fatalf("aurora capability = %+v, %v", c, err)
	}
	if ap.TargetScope() != cloneReceiptScope(auroraCloneCfg()) {
		t.Fatalf("provider scope %q differs from the receipt scope %q", ap.TargetScope(),
			cloneReceiptScope(auroraCloneCfg()))
	}
	bad := auroraCloneCfg()
	bad.Aurora.TTLMinutes = 5
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid aurora block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfAuroraIsTheRegion(t *testing.T) {
	if got := cloneReceiptScope(auroraCloneCfg()); got != "aws/us-east-1" {
		t.Fatalf("aurora receipt scope = %q, want aws/us-east-1", got)
	}
}

type staticCreds struct{ err error }

func (s staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "test"}, s.err
}

func fakeAWSConfig(creds aws.CredentialsProvider) func(context.Context,
	string) (aws.Config, error) {
	return func(_ context.Context, region string) (aws.Config, error) {
		return aws.Config{Region: region, Credentials: creds}, nil
	}
}

// The reaper's adapter has no production pool: any create it is asked
// for is refused before AWS is contacted.
func TestAuroraProviderWithoutProductionNeverCreates(t *testing.T) {
	p, err := clone.NewAuroraCloneProvider(clone.AuroraConfig{Region: "us-east-1",
		TTL: time.Hour, API: auroraRDSAPI("us-east-1", fakeAWSConfig(staticCreds{}))})
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

func TestAuroraRDSAPIChecksCredentialsFirst(t *testing.T) {
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
		if api, err := auroraRDSAPI("us-east-1", load)(ctx); err == nil || api != nil {
			t.Errorf("%s: api = %v, err = %v; want an error", name, api, err)
		}
	}
	api, err := auroraRDSAPI("us-east-1", fakeAWSConfig(staticCreds{}))(ctx)
	if err != nil || api == nil {
		t.Fatalf("with credentials: %v, %v", api, err)
	}
}

func TestAuroraProductionIsTheMonitoredDSN(t *testing.T) {
	const dsn = "postgres://app:pw@shop.cluster-abc123def456.us-east-1.rds.amazonaws.com:" +
		"5432/shop?sslmode=require"
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	got, err := auroraProduction(pool)(context.Background())
	if err != nil || got.DSN != dsn {
		t.Fatalf("production = %+v, %v", got, err)
	}
	if auroraProduction(nil) != nil {
		t.Fatal("a nil pool produced a production source")
	}
}

// No AWS credentials make the rehearsal recommend-only, not an error.
func TestAuroraWithoutCredentialsRehearsalIsRecommendOnly(t *testing.T) {
	p, err := clone.NewAuroraCloneProvider(clone.AuroraConfig{Region: "us-east-1",
		TTL: time.Hour, API: auroraRDSAPI("us-east-1", fakeAWSConfig(staticCreds{
			err: errors.New("no credentials")}))})
	if err != nil {
		t.Fatal(err)
	}
	orch := rehearsal.NewOrchestrator(p, nil, rehearsal.Options{MaxCloneAge: 60e9,
		MaxDuration: rehearsal.DefaultMaxDuration})
	result, err := recommendOnlyOnCloneFailure{orch}.Rehearse(context.Background(),
		planpkg.Plan{ExpandSteps: []planpkg.Step{{SQL: "ALTER TABLE t ADD COLUMN c int"}}})
	if err != nil || result.Verdict != rehearsal.VerdictRecommendOnly ||
		result.Reason != rehearsal.Reason("clone_unavailable") {
		t.Fatalf("credential-less aurora rehearsal = %+v, %v", result, err)
	}
}

func TestGameDayProviderSelectsAuroraClone(t *testing.T) {
	settings := config.DefaultConfig().SRE.Autonomy.GameDays
	settings.Enabled = true
	guard := localTargetGuard{pools: func(context.Context) ([]gameday.Querier, error) {
		return nil, nil
	}}
	p, name, err := gameDayProvider(settings, auroraCloneCfg(), guard)
	if err != nil || name != "aurora_clone" {
		t.Fatalf("game-day aurora provider = %v %q %v", p, name, err)
	}
	if _, ok := p.(*clone.AuroraCloneProvider); !ok {
		t.Fatalf("game days built %T", p)
	}
	b, bname, err := localBenchProvider(config.DefaultConfig().SRE.Autonomy,
		auroraCloneCfg(), guard)
	if err != nil || bname != "aurora_clone" || b == nil {
		t.Fatalf("bench aurora provider = %v %q %v", b, bname, err)
	}
}

// An aurora_clone clone lives at most clone.aurora.ttl_minutes, so a
// rehearsal (clone creation included) ends by then.
func TestRehearsalMaxDurationEndsBeforeTheAuroraTTL(t *testing.T) {
	cases := []struct {
		ttl  int
		want time.Duration
	}{{30, 30 * time.Minute}, {59, 59 * time.Minute}, {60, rehearsal.DefaultMaxDuration},
		{1440, rehearsal.DefaultMaxDuration}, {0, time.Minute}}
	for _, tc := range cases {
		c := auroraCloneCfg()
		c.Aurora.TTLMinutes = tc.ttl
		if got := rehearsalMaxDuration(c); got != tc.want {
			t.Errorf("aurora ttl %d: bound = %s, want %s", tc.ttl, got, tc.want)
		}
	}
	c := auroraCloneCfg()
	c.Neon.TTLMinutes = 5
	if got := rehearsalMaxDuration(c); got != rehearsal.DefaultMaxDuration {
		t.Errorf("aurora with a leftover neon ttl: bound = %s", got)
	}
}
