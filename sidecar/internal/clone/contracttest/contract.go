package contracttest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Subject is one provider under contract.
type Subject struct {
	Adapter    string
	Provider   clone.Provider
	Production clone.ServerIdentity
	// Exists reports whether a resource is still live; nil when the
	// provider cannot be observed (the teardown is then trusted).
	Exists func(ctx context.Context, ref string) (bool, error)
}

// Factory makes a fresh subject for one contract check.
type Factory func(t *testing.T) Subject

// Options is where the contract keeps its receipts.
type Options struct {
	Pool         *pgxpool.Pool // a bootstrapped control database
	DeploymentID string
}

// Run asserts the contract against factory's provider: it describes
// itself, its clones are not production, destroy is idempotent, and a
// clone a killed process left is reaped by the next one.
func Run(t *testing.T, factory Factory, opts Options) {
	t.Run("describe_and_estimate", func(t *testing.T) { checkDescribe(t, factory(t)) })
	t.Run("dsn_not_production", func(t *testing.T) { checkNotProduction(t, factory(t)) })
	t.Run("destroy_idempotent", func(t *testing.T) { checkDestroyTwice(t, factory(t)) })
	t.Run("kill_after_ready_reaped", func(t *testing.T) {
		checkKillAfterReady(t, factory(t), opts)
	})
	t.Run("kill_before_ready_reaped", func(t *testing.T) {
		s := factory(t)
		if _, ok := s.Provider.(clone.Finder); !ok {
			t.Fatalf("%s has no FindByName: a kill between create and ready leaks", s.Adapter)
		}
		rep := killBetweenCreateAndReady(t, s, opts)
		if len(rep.Destroyed) != 1 {
			t.Fatalf("the next process did not reap the clone: %+v", rep)
		}
	})
}

// LiveFlag reports whether the live-provider env flag is set, else why
// the live run is skipped (a skip is listed in the round report).
func LiveFlag(flag string) (bool, string) {
	if strings.TrimSpace(os.Getenv(flag)) != "" {
		return true, ""
	}
	return false, fmt.Sprintf("%s not set; live clone provider contract skipped", flag)
}

// RequireLive skips t unless flag is set.
func RequireLive(t *testing.T, flag string) {
	t.Helper()
	if ok, reason := LiveFlag(flag); !ok {
		t.Skip(reason)
	}
}

// CheckNotProduction is ErrTargetIsProduction when c's DSN names
// production's host and port, and fails closed (ErrTargetUnverified)
// without a DSN or a production identity.
func CheckNotProduction(production clone.ServerIdentity, c clone.Clone) error {
	if production.Host == "" || production.StartedAt.IsZero() {
		return fmt.Errorf("%w: no production identity", clone.ErrTargetUnverified)
	}
	if c.DSN == "" {
		return fmt.Errorf("%w: clone %s has no DSN", clone.ErrTargetUnverified, c.ID)
	}
	parsed, err := pgxpool.ParseConfig(c.DSN)
	if err != nil {
		return fmt.Errorf("parse clone %s DSN: %w", c.ID, err)
	}
	return clone.RefuseProduction(production, clone.ServerIdentity{
		Host: strings.ToLower(parsed.ConnConfig.Host), Port: parsed.ConnConfig.Port})
}

func checkDescribe(t *testing.T, s Subject) {
	ctx := context.Background()
	if _, ok := s.Provider.(clone.Describer); !ok {
		t.Fatalf("%s does not describe its clones", s.Adapter)
	}
	c, err := clone.DescribeOf(ctx, s.Provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clone.CheckOptIn(ctx, s.Provider, true); err != nil {
		t.Fatalf("%s kind %q is not a usable clone kind: %v", s.Adapter, c.Kind, err)
	}
	if c.TypicalReady <= 0 {
		t.Fatalf("%s gives no typical ready time", s.Adapter)
	}
	est := c.Estimate(time.Hour, nil)
	if est.Kind != c.Kind || est.Label == "" || est.Priced {
		t.Fatalf("%s estimate without rates: %+v", s.Adapter, est)
	}
}

func checkNotProduction(t *testing.T, s Subject) {
	c, err := s.Provider.Create(context.Background(),
		clone.CloneSpec{ResourceName: "sage-rehearsal-c0de0001"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Provider.Destroy(context.Background(), c) })
	if err := CheckNotProduction(s.Production, c); err != nil {
		t.Fatalf("%s handed out a clone that may be production: %v", s.Adapter, err)
	}
}

func checkDestroyTwice(t *testing.T, s Subject) {
	ctx := context.Background()
	c, err := s.Provider.Create(ctx, clone.CloneSpec{ResourceName: "sage-rehearsal-c0de0002"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := s.Provider.Destroy(ctx, c); err != nil {
			t.Fatalf("%s destroy %d: %v", s.Adapter, i+1, err)
		}
	}
	assertGone(t, s, c.ID)
}

func assertGone(t *testing.T, s Subject, ref string) {
	t.Helper()
	if s.Exists == nil {
		return
	}
	ok, err := s.Exists(context.Background(), ref)
	if err != nil || ok {
		t.Fatalf("%s clone %s still exists (%v)", s.Adapter, ref, err)
	}
}
