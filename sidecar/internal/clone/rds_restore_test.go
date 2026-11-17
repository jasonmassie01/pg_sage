package clone_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05d: rds_restore restores the monitored RDS for PostgreSQL instance
// to its latest restorable time as a new instance (a full copy), waits for
// it, and hands out the monitored DSN on the new instance's endpoint.

func TestRDSRestore_HappyPathRestoresWaitsThenDeletes(t *testing.T) {
	f := newRDSFake(t)
	f.readyAfter = 3
	p := newRDSProvider(t, f, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: rdsName})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != rdsName || !f.exists(rdsName) {
		t.Fatalf("clone = %+v, instance exists %v", c, f.exists(rdsName))
	}
	if !c.CreatedFrom.Equal(rdsNow.Add(-5 * time.Minute)) {
		t.Fatalf("created from %s, want the latest restorable time", c.CreatedFrom)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"restore " + rdsName + " from shop", "delete " + rdsName}
	if got := f.mutating(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mutating calls = %q, want %q", got, want)
	}
	if f.exists(rdsName) || !f.exists("shop") {
		t.Fatal("destroy left the clone or removed the source")
	}
}

func TestRDSRestore_DSNIsTheMonitoredDSNOnTheNewInstance(t *testing.T) {
	f := newRDSFake(t)
	c, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Hostname() != rdsName+".abc123def456.us-east-1.rds.amazonaws.com" ||
		u.Port() != "5432" || u.User.Username() != "app" || pw != rdsPassword ||
		u.Path != "/shop" || u.Query().Get("sslmode") != "require" {
		t.Fatalf("clone DSN host %s port %s user %s db %s sslmode %q", u.Hostname(),
			u.Port(), u.User.Username(), u.Path, u.Query().Get("sslmode"))
	}
}

func TestRDSRestore_RequestCopiesTheSourceAndTagsTheReceipt(t *testing.T) {
	f := newRDSFake(t)
	if _, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName}); err != nil {
		t.Fatal(err)
	}
	r := f.restores[0]
	if r.ID != rdsName || r.Source != "shop" || r.Class != "db.m6g.large" ||
		r.SubnetGroup != "shop-subnets" || r.ParameterGroup != "shop-params" ||
		strings.Join(r.SecurityGroups, ",") != "sg-0a1,sg-0b2" || r.StorageType != "gp3" ||
		r.Iops != 0 || r.PubliclyAccessible {
		t.Fatalf("restore request = %+v: want the source's class, network and storage", r)
	}
	if r.Tags[clone.AuroraReceiptTag] != rdsName ||
		r.Tags[clone.AuroraExpiresTag] != "2026-11-16T15:15:00Z" || len(r.Tags) != 2 {
		t.Fatalf("restore tags = %v: want the receipt name and expiry only", r.Tags)
	}
}

func TestRDSRestore_InstanceClassAndProvisionedIOPS(t *testing.T) {
	f := newRDSFake(t)
	f.setSource(func(s *clone.RDSInstance) {
		s.StorageType, s.Iops, s.PubliclyAccessible = "io1", 3000, true
	})
	p := newRDSProvider(t, f, func(c *clone.RDSRestoreConfig) {
		c.InstanceClass = "db.t4g.medium"
	})
	if _, err := p.Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName}); err != nil {
		t.Fatal(err)
	}
	r := f.restores[0]
	if r.Class != "db.t4g.medium" || r.StorageType != "io1" || r.Iops != 3000 ||
		!r.PubliclyAccessible {
		t.Fatalf("restore request = %+v: want the configured class, io1 IOPS and the "+
			"source's exposure", r)
	}
}

func TestRDSRestore_DescribeIsAFullCopyThatNeedsTheOptIn(t *testing.T) {
	ctx := context.Background()
	p := newRDSProvider(t, newRDSFake(t), nil)
	c, err := p.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != clone.KindFullCopy || c.TypicalReady <= 0 ||
		c.MaxTTL != 3*time.Hour+15*time.Minute {
		t.Fatalf("capability = %+v", c)
	}
	if _, err := clone.CheckOptIn(ctx, p, false); !errors.Is(err,
		clone.ErrFullCopyNotAllowed) {
		t.Fatalf("opt-in without allow_full_copy = %v", err)
	}
	if _, err := clone.CheckOptIn(ctx, p, true); err != nil {
		t.Fatalf("opt-in with allow_full_copy = %v", err)
	}
	est := c.Estimate(3*time.Hour, &clone.Rate{PerHour: 0.5, Currency: "USD",
		AsOf: "2026-11-01", Source: "shop"})
	if !est.Priced || est.Amount != 1.5 || est.InstanceHours != 3 ||
		est.Kind != clone.KindFullCopy {
		t.Fatalf("priced estimate = %+v", est)
	}
}

// Invalid configuration: without clone.allow_full_copy the receipting
// provider asks RDS for nothing, not even a describe.
func TestRDSRestore_RefusedWithoutAllowFullCopy(t *testing.T) {
	f := newRDSFake(t)
	r, err := clone.NewReceiptingProvider(newRDSProvider(t, f, nil), nil,
		clone.ReceiptingConfig{Adapter: clone.RDSRestoreAdapter, Scope: "aws/us-east-1",
			Deployment: func(context.Context) (string, error) { return "d", nil },
			Purpose:    clone.PurposeRehearsal, TTL: time.Hour,
			Backoff: clone.NewCreateBackoff(nil)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Create(context.Background(), clone.CloneSpec{})
	if !errors.Is(err, clone.ErrFullCopyNotAllowed) {
		t.Fatalf("create without the opt-in = %v", err)
	}
	if n := f.callCount(); n != 0 {
		t.Fatalf("RDS was called %d times without the opt-in", n)
	}
}

func TestRDSRestore_SnapshotAgeIsTheLatestRestorableTime(t *testing.T) {
	age, err := newRDSProvider(t, newRDSFake(t), nil).SnapshotAge(context.Background())
	if err != nil || age != 5*time.Minute {
		t.Fatalf("snapshot age = %s, %v; want 5m", age, err)
	}
}

func TestRDSRestore_TargetScopeIsTheRegion(t *testing.T) {
	if got := newRDSAPI(t, newRDSFake(t), nil).TargetScope(); got != "aws/us-east-1" {
		t.Fatalf("scope = %q", got)
	}
}

func TestRDSRestore_ConfigBounds(t *testing.T) {
	f := newRDSFake(t)
	cases := []struct {
		name   string
		mutate func(*clone.RDSRestoreConfig)
		ok     bool
	}{
		{"defaults", nil, true},
		{"bad region", func(c *clone.RDSRestoreConfig) { c.Region = "US-EAST-1" }, false},
		{"empty region", func(c *clone.RDSRestoreConfig) { c.Region = "" }, false},
		{"bad class", func(c *clone.RDSRestoreConfig) { c.InstanceClass = "m6g.large" }, false},
		{"deadline at min", func(c *clone.RDSRestoreConfig) {
			c.Deadline = clone.RDSRestoreMinDeadline
		}, true},
		{"deadline below min", func(c *clone.RDSRestoreConfig) {
			c.Deadline = clone.RDSRestoreMinDeadline - time.Second
		}, false},
		{"deadline at max", func(c *clone.RDSRestoreConfig) {
			c.Deadline, c.TTL = clone.RDSRestoreMaxDeadline, clone.RDSRestoreMaxTTL
		}, true},
		{"deadline above max", func(c *clone.RDSRestoreConfig) {
			c.Deadline, c.TTL = clone.RDSRestoreMaxDeadline+time.Second, clone.RDSRestoreMaxTTL
		}, false},
		{"ttl equal to deadline", func(c *clone.RDSRestoreConfig) { c.TTL = c.Deadline }, false},
		{"ttl above max", func(c *clone.RDSRestoreConfig) {
			c.TTL = clone.RDSRestoreMaxTTL + time.Second
		}, false},
		{"zero deadline", func(c *clone.RDSRestoreConfig) { c.Deadline = 0 }, false},
	}
	for _, tc := range cases {
		_, err := clone.NewRDSRestoreAPI(rdsConfig(f, tc.mutate))
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, clone.ErrInvalidRDSRestoreConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidRDSRestoreConfig", tc.name, err)
		}
	}
}

// Concurrency: two restores at once get two instances and two requests.
func TestRDSRestore_TwoRestoresGetTwoNames(t *testing.T) {
	f := newRDSFake(t)
	p := newRDSProvider(t, f, nil)
	names := []string{"sage-rehearsal-0000000a", "sage-rehearsal-0000000b"}
	got := make([]clone.Clone, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = p.Create(context.Background(), clone.CloneSpec{ResourceName: name})
		}()
	}
	wg.Wait()
	for i, name := range names {
		if errs[i] != nil || got[i].ID != name || !f.exists(name) {
			t.Fatalf("create %s = %+v, %v", name, got[i], errs[i])
		}
	}
	if got[0].DSN == got[1].DSN || len(f.restores) != 2 {
		t.Fatalf("two restores share a DSN or were not both sent: %d", len(f.restores))
	}
}

func TestRDSRestore_FindByNameNeedsTheReceiptTag(t *testing.T) {
	ctx := context.Background()
	f := newRDSFake(t)
	a := newRDSAPI(t, f, nil)
	if ref, found, err := a.FindByName(ctx, rdsName); err != nil || found || ref != "" {
		t.Fatalf("find before create = %q, %v, %v", ref, found, err)
	}
	f.put(clone.RDSInstance{ID: rdsName, Engine: "postgres", Status: "available",
		Tags: map[string]string{clone.AuroraReceiptTag: rdsName}})
	if ref, found, err := a.FindByName(ctx, rdsName); err != nil || !found || ref != rdsName {
		t.Fatalf("find tagged = %q, %v, %v", ref, found, err)
	}
	f.put(clone.RDSInstance{ID: rdsName, Engine: "postgres", Status: "available"})
	if _, found, err := a.FindByName(ctx, rdsName); err != nil || found {
		t.Fatalf("an untagged instance of the same name was found: %v, %v", found, err)
	}
	if _, _, err := a.FindByName(ctx, "shop"); !errors.Is(err,
		clone.ErrInvalidRDSRestoreConfig) {
		t.Fatalf("find of a name pg_sage never gives = %v", err)
	}
	f.describeErr = fmt.Errorf("aurora: %w", &clone.ProviderStatusError{Op: "x",
		StatusCode: 500})
	if _, _, err := a.FindByName(ctx, rdsName); err == nil {
		t.Fatal("a describe failure was reported as not found")
	}
}
