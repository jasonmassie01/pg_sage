package contracttest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// lab-05a: the contract every clone provider meets, run here against the
// fakes; lab-05b to lab-05g run it against their live providers behind
// their env flags.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/clone/contracttest"))
}

const contractDeployment = "00000000-0000-4000-8000-0000000c0a7a"

var production = clone.ServerIdentity{Host: "prod.db.invalid", Port: 5432,
	StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

func contractPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestContract_FakeProvidersMeetTheContract(t *testing.T) {
	pool := contractPool(t)
	for _, kind := range []clone.Kind{clone.KindThinClone, clone.KindBranch,
		clone.KindCopyOnWrite, clone.KindFullCopy} {
		t.Run(string(kind), func(t *testing.T) {
			Run(t, func(t *testing.T) Subject {
				fake := NewFake(kind, "clone.db.invalid")
				return Subject{Adapter: "contract-" + string(kind), Provider: fake,
					Production: production, Exists: fake.Exists}
			}, Options{Pool: pool, DeploymentID: contractDeployment})
		})
	}
}

func TestContract_WithoutFinderAKillIsReportedNotGuessed(t *testing.T) {
	pool := contractPool(t)
	fake := NewFake(clone.KindBranch, "clone.db.invalid")
	got := killBetweenCreateAndReady(t, Subject{Adapter: "contract-nofinder",
		Provider: noFinder{fake}, Production: production, Exists: fake.Exists},
		Options{Pool: pool, DeploymentID: contractDeployment})
	if _, ok := any(noFinder{fake}).(clone.Finder); ok {
		t.Fatal("the test double still has FindByName")
	}
	if len(got.Destroyed) != 0 || len(got.Unresolved) != 1 {
		t.Fatalf("a provider without Finder: %+v", got)
	}
}

// noFinder is the fake without its FindByName.
type noFinder struct{ fake *Fake }

func (n noFinder) Create(ctx context.Context, s clone.CloneSpec) (clone.Clone, error) {
	return n.fake.Create(ctx, s)
}
func (n noFinder) Destroy(ctx context.Context, c clone.Clone) error {
	return n.fake.Destroy(ctx, c)
}
func (n noFinder) SnapshotAge(ctx context.Context) (time.Duration, error) {
	return n.fake.SnapshotAge(ctx)
}
func (n noFinder) Describe(ctx context.Context) (clone.Capability, error) {
	return n.fake.Describe(ctx)
}

func TestContract_ProductionDSNIsRefusedForEveryAdapter(t *testing.T) {
	names := clone.AdapterNames()
	for _, want := range []string{"dle", "lakebase_branch", "rds_restore", "cloudsql_clone",
		"alloydb_restore", "azure_restore"} {
		if !contains(names, want) {
			t.Errorf("adapter %s is not registered: %v", want, names)
		}
	}
	for _, name := range names {
		fake := NewFake(clone.KindBranch, production.Host)
		created, err := fake.Create(context.Background(), clone.CloneSpec{ResourceName: name})
		if err != nil {
			t.Fatal(err)
		}
		err = CheckNotProduction(production, created)
		if !errors.Is(err, clone.ErrTargetIsProduction) {
			t.Errorf("%s: a clone on production's DSN was not refused: %v", name, err)
		}
	}
}

func TestContract_CheckNotProductionFailsClosed(t *testing.T) {
	if err := CheckNotProduction(production, clone.Clone{ID: "x"}); !errors.Is(err,
		clone.ErrTargetUnverified) {
		t.Fatalf("no DSN: %v", err)
	}
	if err := CheckNotProduction(production, clone.Clone{ID: "x",
		DSN: "postgres://%zz"}); err == nil {
		t.Fatal("an unparsable DSN was accepted")
	}
	if err := CheckNotProduction(clone.ServerIdentity{}, clone.Clone{ID: "x",
		DSN: "postgres://clone.db.invalid/app"}); !errors.Is(err, clone.ErrTargetUnverified) {
		t.Fatalf("no production identity: %v", err)
	}
	other := clone.Clone{ID: "x", DSN: "postgres://clone.db.invalid:5432/app"}
	if err := CheckNotProduction(production, other); err != nil {
		t.Fatalf("a clone on another host was refused: %v", err)
	}
	samePortOtherHost := clone.Clone{ID: "x", DSN: "postgres://PROD.db.invalid:5432/app"}
	if err := CheckNotProduction(production, samePortOtherHost); !errors.Is(err,
		clone.ErrTargetIsProduction) {
		t.Fatalf("production's host in another case was accepted: %v", err)
	}
}

func TestContract_LiveFlagNamesTheFlag(t *testing.T) {
	const flag = "SAGE_TEST_CONTRACT_FLAG_LAB05A"
	t.Setenv(flag, "")
	if ok, reason := LiveFlag(flag); ok || !strings.Contains(reason, flag) {
		t.Fatalf("unset flag: %v, %q", ok, reason)
	}
	t.Setenv(flag, "1")
	if ok, reason := LiveFlag(flag); !ok || reason != "" {
		t.Fatalf("set flag: %v, %q", ok, reason)
	}
}

func TestContract_FakeDestroyIsIdempotentAndFinds(t *testing.T) {
	fake := NewFake(clone.KindBranch, "clone.db.invalid")
	ctx := context.Background()
	c, err := fake.Create(ctx, clone.CloneSpec{ResourceName: "sage-rehearsal-0000aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	ref, found, err := fake.FindByName(ctx, "sage-rehearsal-0000aaaa")
	if err != nil || !found || ref != c.ID {
		t.Fatalf("find: %q %v %v", ref, found, err)
	}
	for range 2 {
		if err := fake.Destroy(ctx, c); err != nil {
			t.Fatalf("destroy: %v", err)
		}
	}
	if ok, _ := fake.Exists(ctx, c.ID); ok {
		t.Fatal("destroyed clone exists")
	}
	if _, found, _ := fake.FindByName(ctx, "sage-rehearsal-0000aaaa"); found {
		t.Fatal("a destroyed clone was found")
	}
	if _, err := fake.Create(ctx, clone.CloneSpec{}); err == nil {
		t.Fatal("a create without a resource name succeeded")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
