package gameday

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Game days (AI-SRE-SPEC §4 R3): PGIncidentBench fault programs run on a
// disposable clone of the customer's database (the existing clone
// provider, or a local development database), never on a monitored
// database. The clone is always destroyed; the report is ingested as
// game-day evidence; a forbidden action is a family safety violation.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/gameday"))
}

var (
	poolOnce sync.Once
	shared   *pgxpool.Pool
	poolErr  error
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		shared, poolErr = pgxpool.New(context.Background(), dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(context.Background(), shared)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	return shared
}

func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type fakeProvider struct {
	mu         sync.Mutex
	createErr  error
	destroyErr error
	created    []clone.Clone
	destroyed  []clone.Clone
	specs      []clone.CloneSpec
}

func (p *fakeProvider) Create(_ context.Context, spec clone.CloneSpec) (clone.Clone, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.specs = append(p.specs, spec)
	if p.createErr != nil {
		return clone.Clone{}, p.createErr
	}
	c := clone.Clone{ID: fmt.Sprintf("clone-%d", len(p.created)+1),
		DSN: "postgres://gameday:secret@clone.internal:5432/orders", CreatedFrom: time.Now()}
	p.created = append(p.created, c)
	return c, nil
}

func (p *fakeProvider) Destroy(_ context.Context, c clone.Clone) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, c)
	return p.destroyErr
}

func (p *fakeProvider) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

type fakeFaults struct {
	mu       sync.Mutex
	report   []byte
	err      error
	dsns     []string
	families [][]string
	block    chan struct{}
}

func (f *fakeFaults) Run(ctx context.Context, dsn string, families []string) ([]byte, error) {
	f.mu.Lock()
	f.dsns = append(f.dsns, dsn)
	f.families = append(f.families, families)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.report, f.err
}

func report(at time.Time, forbidden int) []byte {
	return []byte(fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1",
		"generated_at": %q, "gated_arms": ["causal-graph"], "cells": [
		{"arm": "causal-graph", "family": "lock_blocking", "runs": 11,
		 "safe_pass": {"k": 11, "n": 11}, "top1": {"k": 7, "n": 7},
		 "mechanism_precision": 1, "forbidden_actions": %d}]}`,
		at.UTC().Format(time.RFC3339), forbidden))
}

type gdFixture struct {
	t        *testing.T
	ctx      context.Context
	ledger   *earned.Service
	store    *Store
	provider *fakeProvider
	faults   *fakeFaults
	now      time.Time
	database string
}

func newGDFixture(t *testing.T) *gdFixture {
	t.Helper()
	pool := testPool(t)
	dep := newUUID(t)
	database := "orders-" + newUUID(t)[:8]
	es, err := earned.NewPostgresStore(pool, dep, database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := &gdFixture{t: t, ctx: context.Background(), now: now}
	cfg := earned.DefaultConfig()
	// The ledger shares the fixture clock, so advancing f.now moves both.
	cfg.Now = func() time.Time { return f.now }
	ledger, err := earned.NewService(es, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(pool, dep)
	if err != nil {
		t.Fatal(err)
	}
	f.ledger, f.store, f.provider = ledger, st, &fakeProvider{}
	f.faults = &fakeFaults{report: report(now, 0)}
	f.database = database
	return f
}

func (f *gdFixture) runner(families ...string) *Runner {
	f.t.Helper()
	r, err := NewRunner(Config{Database: f.database, Provider: "dle", Families: families,
		Interval: 7 * 24 * time.Hour, Now: func() time.Time { return f.now }},
		f.provider, f.faults, f.ledger, f.store)
	if err != nil {
		f.t.Fatalf("runner: %v", err)
	}
	return r
}

func TestGameDayRunsOnACloneAndIngestsTheReport(t *testing.T) {
	f := newGDFixture(t)
	gd, err := f.runner("lock_blocking").Run(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gd.Status != StatusCompleted || gd.EvalRunID == "" || gd.CloneID != "clone-1" ||
		gd.FinishedAt == nil || gd.Error != "" || gd.Provider != "dle" {
		t.Fatalf("game day = %+v", gd)
	}
	if len(f.faults.dsns) != 1 || f.faults.dsns[0] != f.provider.created[0].DSN ||
		strings.Join(f.faults.families[0], ",") != "lock_blocking" {
		t.Fatalf("faults ran on %v %v", f.faults.dsns, f.faults.families)
	}
	if len(f.provider.destroyed) != 1 || f.provider.destroyed[0].ID != "clone-1" ||
		!f.provider.specs[0].IncludeData {
		t.Fatalf("clone lifecycle: created %v destroyed %v", f.provider.created,
			f.provider.destroyed)
	}
	runs, err := f.ledger.Store().GameDayRuns(f.ctx, f.now.Add(-time.Hour))
	if err != nil || len(runs) != 1 || runs[0].ID != gd.EvalRunID ||
		runs[0].Database != f.database || runs[0].Source != earned.SourceGameDay {
		t.Fatalf("ingested runs = %+v (%v)", runs, err)
	}
	listed, err := f.runner().List(f.ctx, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != gd.ID {
		t.Fatalf("list = %+v (%v)", listed, err)
	}
}

// The clone's DSN carries credentials: it is never stored or returned.
func TestGameDayNeverKeepsTheCloneDSN(t *testing.T) {
	f := newGDFixture(t)
	gd, err := f.runner().Run(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := f.runner().List(f.ctx, 10)
	for _, s := range []string{fmt.Sprintf("%+v", gd), fmt.Sprintf("%+v", listed)} {
		if strings.Contains(s, "secret") || strings.Contains(s, "clone.internal") {
			t.Fatalf("the clone DSN leaked: %s", s)
		}
	}
}

func TestGameDayFailuresStillDestroyTheClone(t *testing.T) {
	f := newGDFixture(t)
	f.faults.err = errors.New("fault program broke")
	gd, err := f.runner().Run(f.ctx)
	if err == nil || gd.Status != StatusFailed || !strings.Contains(gd.Error,
		"fault program broke") || len(f.provider.destroyed) != 1 {
		t.Fatalf("game day = %+v (%v), destroyed %d", gd, err, len(f.provider.destroyed))
	}
	f.faults.err = nil
	f.faults.report = []byte(`{"schema": "nope"}`)
	gd, err = f.runner().Run(f.ctx)
	if !errors.Is(err, earned.ErrInvalidReport) || gd.Status != StatusFailed ||
		len(f.provider.destroyed) != 2 {
		t.Fatalf("invalid report: %+v (%v)", gd, err)
	}
}

func TestGameDayCreateFailureDestroysNothing(t *testing.T) {
	f := newGDFixture(t)
	f.provider.createErr = errors.New("DLE create failed with status 503")
	gd, err := f.runner().Run(f.ctx)
	if err == nil || gd.Status != StatusFailed || len(f.provider.destroyed) != 0 ||
		len(f.faults.dsns) != 0 || !strings.Contains(gd.Error, "503") {
		t.Fatalf("game day = %+v (%v)", gd, err)
	}
}

func TestGameDayDestroyFailureIsRecorded(t *testing.T) {
	f := newGDFixture(t)
	f.provider.destroyErr = errors.New("DLE destroy failed")
	gd, err := f.runner().Run(f.ctx)
	if err == nil || gd.Status != StatusDestroyFailed || gd.EvalRunID == "" ||
		!strings.Contains(gd.Error, "destroy") {
		t.Fatalf("game day = %+v (%v): evidence kept, clone leak reported", gd, err)
	}
}

// A forbidden action on a game day is a family safety violation.
func TestGameDayForbiddenActionDemotesTheFamily(t *testing.T) {
	f := newGDFixture(t)
	f.faults.report = report(f.now, 1)
	if _, err := f.runner().Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	n, err := f.ledger.Store().FamilyViolations(f.ctx, earned.FamilyLockBlocking,
		f.now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("lock_blocking violations = %d (%v)", n, err)
	}
}

func TestGameDayIsSingleFlight(t *testing.T) {
	f := newGDFixture(t)
	f.faults.block = make(chan struct{})
	r := f.runner()
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(f.ctx)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.faults.mu.Lock()
		started := len(f.faults.dsns) == 1
		f.faults.mu.Unlock()
		if started || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := r.Run(f.ctx); !errors.Is(err, ErrRunning) {
		t.Fatalf("second concurrent run: %v", err)
	}
	close(f.faults.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGameDayRunDueHonorsTheInterval(t *testing.T) {
	f := newGDFixture(t)
	r := f.runner()
	if _, ran, err := r.RunDue(f.ctx); err != nil || !ran {
		t.Fatalf("first due run: ran=%v err=%v", ran, err)
	}
	f.now = f.now.Add(6 * 24 * time.Hour)
	if _, ran, err := r.RunDue(f.ctx); err != nil || ran {
		t.Fatalf("run before the interval: ran=%v err=%v", ran, err)
	}
	f.now = f.now.Add(25 * time.Hour)
	f.faults.report = report(f.now, 0)
	if _, ran, err := r.RunDue(f.ctx); err != nil || !ran {
		t.Fatalf("run after the interval: ran=%v err=%v", ran, err)
	}
}

func TestNewRunnerRequiresAProvider(t *testing.T) {
	f := newGDFixture(t)
	_, err := NewRunner(Config{Database: f.database}, nil, f.faults, f.ledger, f.store)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no provider: %v", err)
	}
	if _, err := NewRunner(Config{}, f.provider, f.faults, f.ledger, f.store); err == nil {
		t.Fatal("a runner without a database name was built")
	}
	if _, err := NewRunner(Config{Database: f.database, Families: []string{"Bad Name"}},
		f.provider, f.faults, f.ledger, f.store); err == nil {
		t.Fatal("an invalid family name was accepted")
	}
}

func TestLocalProviderRefusesMonitoredDatabases(t *testing.T) {
	monitored := []string{"postgres://app:pw@db.prod:5432/orders",
		"host=10.0.0.5 port=6432 dbname=billing user=sage"}
	for _, dsn := range []string{
		"postgres://other:x@db.prod:5432/orders",
		"postgres://app@DB.PROD:5432/orders?sslmode=require",
		"host=10.0.0.5 port=6432 dbname=billing user=someone",
	} {
		if _, err := NewLocalProvider(dsn, monitored); !errors.Is(err, ErrMonitoredDSN) {
			t.Errorf("%s: %v", dsn, err)
		}
	}
	p, err := NewLocalProvider("postgres://u:p@127.0.0.1:5999/scratch", monitored)
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Create(context.Background(), clone.CloneSpec{IncludeData: true})
	if err != nil || c.DSN != "postgres://u:p@127.0.0.1:5999/scratch" || c.ID != "local" {
		t.Fatalf("local clone = %+v (%v)", c, err)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatalf("destroy is a no-op for the local database: %v", err)
	}
	if _, err := NewLocalProvider("", monitored); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty dsn: %v", err)
	}
	if _, err := NewLocalProvider("::nope::", monitored); err == nil {
		t.Fatal("invalid dsn accepted")
	}
}

// An operator-started game day runs in the background (it takes minutes)
// and refuses a second start while it runs.
func TestGameDayStartRunsInTheBackground(t *testing.T) {
	f := newGDFixture(t)
	f.faults.block = make(chan struct{})
	r := f.runner()
	if err := r.Start(f.ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.faults.mu.Lock()
		started := len(f.faults.dsns) == 1
		f.faults.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background game day never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.Start(f.ctx); !errors.Is(err, ErrRunning) {
		t.Fatalf("second start: %v", err)
	}
	close(f.faults.block)
	for {
		list, err := r.List(f.ctx, 1)
		if err == nil && len(list) == 1 && list[0].Status == StatusCompleted {
			break
		}
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatalf("background game day did not complete: %+v (%v)", list, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRegistryResolvesRunnersByDatabase(t *testing.T) {
	f := newGDFixture(t)
	reg := NewRegistry()
	r := f.runner()
	reg.Register(f.database, r)
	if got, ok := reg.Lookup(f.database); !ok || got != r {
		t.Fatalf("lookup = %v %v", got, ok)
	}
	if _, ok := reg.Lookup("other"); ok {
		t.Fatal("unknown database resolved")
	}
	reg.Register("nil-runner", nil)
	if _, ok := reg.Lookup("nil-runner"); ok {
		t.Fatal("a nil runner was registered")
	}
	reg.Remove(f.database)
	if _, ok := reg.Lookup(f.database); ok {
		t.Fatal("removed runner still resolves")
	}
}
