package clone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05d: rds_restore refuses what is not an RDS for PostgreSQL source,
// is unavailable without credentials or a production identity, leaves a
// restore past its deadline to the reaper, and deletes only what it made.

func TestRDSRestore_RefusesASourceThatIsNotAnRDSInstance(t *testing.T) {
	cases := map[string]string{
		"aurora cluster endpoint": "shop.cluster-abc123def456.us-east-1.rds.amazonaws.com",
		"rds proxy":               "shop.proxy-abc123def456.us-east-1.rds.amazonaws.com",
		"custom dns":              "db.shop.example.com",
		"ip address":              "10.0.0.5",
	}
	for name, host := range cases {
		f := newRDSFake(t)
		p := newRDSProvider(t, f, func(c *clone.RDSRestoreConfig) {
			c.Production = func(context.Context) (clone.RDSProduction, error) {
				return clone.RDSProduction{DSN: "postgres://app:pw@" + host + ":5432/shop"}, nil
			}
		})
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: rdsName})
		if !errors.Is(err, clone.ErrNotRDSInstance) || strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: err = %v, want ErrNotRDSInstance without the DSN", name, err)
		}
		if got := f.mutating(); len(got) != 0 {
			t.Errorf("%s: mutating calls %q", name, got)
		}
	}
}

func TestRDSRestore_RefusesAnAuroraMemberOrAnotherEngine(t *testing.T) {
	cases := map[string]func(*clone.RDSInstance){
		"aurora member": func(s *clone.RDSInstance) {
			s.Engine, s.ClusterID = "aurora-postgresql", "shop-cluster"
		},
		"mysql": func(s *clone.RDSInstance) { s.Engine = "mysql" },
		"other endpoint": func(s *clone.RDSInstance) {
			s.Endpoint = "x.abc.us-east-1.rds.amazonaws.com"
		},
		"multi-az member": func(s *clone.RDSInstance) { s.ClusterID = "shop-maz" },
	}
	for name, mutate := range cases {
		f := newRDSFake(t)
		f.setSource(mutate)
		_, err := newRDSProvider(t, f, nil).Create(context.Background(),
			clone.CloneSpec{ResourceName: rdsName})
		if !errors.Is(err, clone.ErrNotRDSInstance) {
			t.Errorf("%s: err = %v, want ErrNotRDSInstance", name, err)
		}
		if got := f.mutating(); len(got) != 0 {
			t.Errorf("%s: mutating calls %q", name, got)
		}
	}
}

func TestRDSRestore_RefusesARegionMismatchAndABadName(t *testing.T) {
	f := newRDSFake(t)
	p := newRDSProvider(t, f, func(c *clone.RDSRestoreConfig) { c.Region = "eu-west-2" })
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, clone.ErrInvalidRDSRestoreConfig) || !strings.Contains(err.Error(),
		"us-east-1") {
		t.Fatalf("region mismatch = %v", err)
	}
	for _, name := range []string{"", "shop", "sage-rehearsal-XYZ", "prod-db-0a1b2c3d"} {
		_, err := newRDSProvider(t, f, nil).Create(context.Background(),
			clone.CloneSpec{ResourceName: name})
		if !errors.Is(err, clone.ErrInvalidRDSRestoreConfig) {
			t.Errorf("name %q: err = %v", name, err)
		}
	}
	if got := f.mutating(); len(got) != 0 {
		t.Fatalf("mutating calls %q", got)
	}
}

// Nil/empty: no credentials, no production identity, no automated backups.
func TestRDSRestore_UnavailableWithoutCredentialsOrProduction(t *testing.T) {
	ctx := context.Background()
	f := newRDSFake(t)
	cases := map[string]func(*clone.RDSRestoreConfig){
		"nil api": func(c *clone.RDSRestoreConfig) { c.API = nil },
		"no credentials": func(c *clone.RDSRestoreConfig) {
			c.API = func(context.Context) (clone.RDSRestoreClient, error) {
				return nil, errors.New("no EC2 IMDS role found")
			}
		},
		"nil client": func(c *clone.RDSRestoreConfig) {
			c.API = func(context.Context) (clone.RDSRestoreClient, error) { return nil, nil }
		},
		"no production": func(c *clone.RDSRestoreConfig) { c.Production = nil },
		"empty dsn": func(c *clone.RDSRestoreConfig) {
			c.Production = func(context.Context) (clone.RDSProduction, error) {
				return clone.RDSProduction{DSN: "  "}, nil
			}
		},
	}
	for name, mutate := range cases {
		_, err := newRDSProvider(t, f, mutate).Create(ctx,
			clone.CloneSpec{ResourceName: rdsName})
		if !errors.Is(err, clone.ErrCloneUnavailable) {
			t.Errorf("%s: err = %v, want ErrCloneUnavailable", name, err)
		}
	}
	if got := f.mutating(); len(got) != 0 {
		t.Fatalf("mutating calls %q", got)
	}
	// The reaper's adapter has no production but still destroys.
	f.put(clone.RDSInstance{ID: rdsName, Engine: "postgres", Status: "available",
		Tags: map[string]string{clone.AuroraReceiptTag: rdsName}})
	reaper := newRDSProvider(t, f, func(c *clone.RDSRestoreConfig) { c.Production = nil })
	if err := reaper.Destroy(ctx, clone.Clone{ID: rdsName}); err != nil || f.exists(rdsName) {
		t.Fatalf("destroy without production = %v (exists %v)", err, f.exists(rdsName))
	}
}

func TestRDSRestore_RefusesASourceWithoutAutomatedBackups(t *testing.T) {
	f := newRDSFake(t)
	f.setSource(func(s *clone.RDSInstance) { s.LatestRestorableTime = time.Time{} })
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, clone.ErrNoRestorableTime) || !strings.Contains(err.Error(),
		"automated backups") {
		t.Fatalf("err = %v, want ErrNoRestorableTime naming automated backups", err)
	}
	if got := f.mutating(); len(got) != 0 {
		t.Fatalf("mutating calls %q", got)
	}
}

// Boundary: a restore not available by clone.restore_deadline is left to
// the reaper (an uncertain create), and is found by its tag.
func TestRDSRestore_WaitPastTheDeadlineIsLeftForTheReaper(t *testing.T) {
	f := newRDSFake(t)
	f.neverReady = true
	a := newRDSAPI(t, f, nil)
	p, err := clone.NewSnapshotProvider(a, func() time.Time { return rdsNow })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = p.Create(ctx, clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(),
		clone.AuroraReceiptTag) {
		t.Fatalf("err = %v, want the deadline naming the tag the reaper finds", err)
	}
	if !f.exists(rdsName) {
		t.Fatal("the restore past its deadline was deleted, not left for the reaper")
	}
	if ref, found, err := p.FindByName(context.Background(), rdsName); err != nil ||
		!found || ref != rdsName {
		t.Fatalf("find by tag = %q, %v, %v", ref, found, err)
	}
}

// The provider's own deadline (clone.restore_deadline) bounds the wait
// even when the caller's context has none.
func TestRDSRestore_TheRestoreDeadlineBoundsTheWait(t *testing.T) {
	f := newRDSFake(t)
	f.neverReady = true
	a := newRDSAPI(t, f, func(c *clone.RDSRestoreConfig) {
		c.Deadline = clone.RDSRestoreMinDeadline
	})
	clone.SetRDSRestoreDeadlineForTest(a, 30*time.Millisecond)
	start := time.Now()
	_, err := a.Restore(context.Background(), clone.Snapshot{ID: "shop",
		CreatedAt: rdsNow.Add(-5 * time.Minute)}, clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %s, want the restore deadline", err, time.Since(start))
	}
}

func TestRDSRestore_ARefusedRestoreMadeNothing(t *testing.T) {
	f := newRDSFake(t)
	f.restoreErr = &clone.ProviderStatusError{Op: "rds RestoreDBInstanceToPointInTime",
		StatusCode: 400}
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 400 ||
		errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, want the 400 and nothing left behind", err)
	}
	if got := f.mutating(); len(got) != 1 {
		t.Fatalf("mutating calls %q, want the restore only", got)
	}
}

func TestRDSRestore_A5xxAfterTheRestoreDeletesWhatItFinds(t *testing.T) {
	f := newRDSFake(t)
	f.restoreThenErr = &clone.ProviderStatusError{Op: "restore", StatusCode: 503}
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if err == nil || errors.Is(err, clone.ErrCloneLeftBehind) || f.exists(rdsName) {
		t.Fatalf("err = %v, exists %v: want the 503 and the instance deleted", err,
			f.exists(rdsName))
	}
}

func TestRDSRestore_A5xxWithNothingListedStaysUncertain(t *testing.T) {
	f := newRDSFake(t)
	f.restoreErr = &clone.ProviderStatusError{Op: "restore", StatusCode: 500}
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, want an uncertain create (ErrCloneLeftBehind)", err)
	}
}

func TestRDSRestore_AFailedInstanceIsDeleted(t *testing.T) {
	f := newRDSFake(t)
	f.endStatus = "incompatible-parameters"
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if err == nil || !strings.Contains(err.Error(), "incompatible-parameters") ||
		f.exists(rdsName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(rdsName))
	}
}

func TestRDSRestore_ACloneOnProductionsHostIsRefusedAndDeleted(t *testing.T) {
	f := newRDSFake(t)
	f.cloneEndpoint = strings.ToUpper(rdsProdHost)
	_, err := newRDSProvider(t, f, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: rdsName})
	if !errors.Is(err, clone.ErrCloneIsProduction) || f.exists(rdsName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(rdsName))
	}
}

func TestRDSRestore_DestroyDeletesOnlyWhatItMade(t *testing.T) {
	ctx := context.Background()
	f := newRDSFake(t)
	p := newRDSProvider(t, f, nil)
	f.put(clone.RDSInstance{ID: "sage-rehearsal-0000dead", Engine: "postgres",
		Status: "available"})
	f.put(clone.RDSInstance{ID: "sage-rehearsal-0000beef", Engine: "postgres",
		Status: "available", DeletionProtection: true,
		Tags: map[string]string{clone.AuroraReceiptTag: "sage-rehearsal-0000beef"}})
	for _, id := range []string{"shop", "sage-rehearsal-0000dead", "sage-rehearsal-0000beef"} {
		if err := p.Destroy(ctx, clone.Clone{ID: id}); err == nil || !f.exists(id) {
			t.Errorf("destroy %s = %v (exists %v), want a refusal", id, err, f.exists(id))
		}
	}
	if got := f.mutating(); len(got) != 0 {
		t.Fatalf("mutating calls %q", got)
	}
	if err := p.Destroy(ctx, clone.Clone{ID: rdsName}); err != nil {
		t.Fatalf("destroy of a gone instance = %v, want nil (idempotent)", err)
	}
	if err := p.Destroy(ctx, clone.Clone{}); err == nil {
		t.Fatal("destroy without an id succeeded")
	}
}

// Error propagation: a delete RDS refuses is returned, and an instance
// already deleting counts as deleted.
func TestRDSRestore_DeleteFailureIsReturned(t *testing.T) {
	ctx := context.Background()
	f := newRDSFake(t)
	p := newRDSProvider(t, f, nil)
	c, err := p.Create(ctx, clone.CloneSpec{ResourceName: rdsName})
	if err != nil {
		t.Fatal(err)
	}
	f.deleteErr = &clone.ProviderStatusError{Op: "delete", StatusCode: 403}
	if err := p.Destroy(ctx, c); err == nil || !f.exists(rdsName) {
		t.Fatalf("destroy = %v while the delete failed", err)
	}
	f.deleteErr = nil
	f.setSourceStatus(rdsName, "deleting")
	if err := p.Destroy(ctx, c); err != nil {
		t.Fatalf("destroy of a deleting instance = %v", err)
	}
	if got := f.mutating(); strings.Count(strings.Join(got, "|"), "delete ") != 1 {
		t.Fatalf("mutating calls %q: a deleting instance was deleted again", got)
	}
}

func (f *rdsFake) setSourceStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[id].Status = status
}
