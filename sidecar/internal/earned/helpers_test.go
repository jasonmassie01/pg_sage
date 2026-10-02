package earned

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/earned"))
}

var (
	poolOnce sync.Once
	sharedDB *pgxpool.Pool
	poolErr  error
)

// testPool is the package's bootstrapped fixture database.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		ctx := context.Background()
		sharedDB, poolErr = pgxpool.New(ctx, dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(ctx, sharedDB)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	return sharedDB
}

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(at time.Time) *clock { return &clock{now: at} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

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

// fixture is one isolated ledger: its own deployment id on the shared
// fixture database, and a settable clock.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *PostgresStore
	svc   *Service
	clock *clock
}

var fixtureEpoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testPool(t)
	store, err := NewPostgresStore(pool, newUUID(t))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	clk := newClock(fixtureEpoch)
	cfg := DefaultConfig()
	cfg.Now = clk.Now
	cfg.EvidenceCacheTTL = 0
	svc, err := NewService(store, cfg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return &fixture{t: t, ctx: context.Background(), pool: pool, store: store, svc: svc,
		clock: clk}
}

// benchReport renders a PGIncidentBench report with one passing gated
// cell per family, generated at.
func benchReport(at time.Time, families ...Family) []byte {
	cells := make([]string, 0, len(families))
	for _, f := range families {
		cells = append(cells, fmt.Sprintf(`{"arm": "causal-graph", "family": %q,
			"runs": 12, "safe_pass": {"k": 12, "n": 12}, "top1": {"k": 11, "n": 12},
			"mechanism_precision": 0.96, "forbidden_actions": 0}`, f))
	}
	return []byte(fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1",
		"generated_at": %q, "gated_arms": ["causal-graph"], "cells": [%s]}`,
		at.UTC().Format(time.RFC3339), strings.Join(cells, ",")))
}

// seedL2Evidence ingests a fresh bench report and a 31-day shadow record
// of n accepted packets (n >= 20) for family.
func (f *fixture) seedL2Evidence(family Family, accepted, rejected int) {
	f.t.Helper()
	now := f.clock.Now()
	f.clock.Set(now.Add(-31 * 24 * time.Hour))
	for i := 0; i < accepted+rejected; i++ {
		verdict := VerdictAccepted
		if i >= accepted {
			verdict = VerdictRejected
		}
		if i == 1 {
			f.clock.Set(now.Add(-time.Hour))
		}
		if err := f.svc.RecordReview(f.ctx, Review{Database: "db1",
			InvestigationID: newUUID(f.t), Family: family, Verdict: verdict,
			Reviewer: "user:7:ops@example.com"}); err != nil {
			f.t.Fatalf("review: %v", err)
		}
	}
	f.clock.Set(now)
	if _, err := f.svc.IngestEvalRun(f.ctx, benchReport(now.Add(-time.Hour), family),
		SourceBench, "user:1:admin@example.com", ""); err != nil {
		f.t.Fatalf("ingest bench: %v", err)
	}
}

// seedL2Recoveries records n verified live recoveries at L2.
func (f *fixture) seedL2Recoveries(family Family, class ActionClass, n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if err := f.svc.RecordOutcome(f.ctx, Outcome{Database: "db1",
			ActionLogID: int64(100000 + i), Family: family, Class: class, Level: L2,
			Result: ResultVerifiedRecovery, Source: SourceExecutor,
			Actor: "pg_sage"}); err != nil {
			f.t.Fatalf("outcome: %v", err)
		}
	}
}

// promote proposes and approves the next level for the pair.
func (f *fixture) promote(family Family, class ActionClass) State {
	f.t.Helper()
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		f.t.Fatalf("propose: %v", err)
	}
	p := f.pending(family, class)
	if p == nil {
		f.t.Fatalf("no pending proposal for %s/%s", family, class)
	}
	st, err := f.svc.Approve(f.ctx, p.ID, "user:1:admin@example.com", "evidence reviewed")
	if err != nil {
		f.t.Fatalf("approve: %v", err)
	}
	return st
}

func (f *fixture) pending(family Family, class ActionClass) *Proposal {
	f.t.Helper()
	ps, err := f.svc.PendingProposals(f.ctx)
	if err != nil {
		f.t.Fatalf("pending: %v", err)
	}
	for i := range ps {
		if ps[i].Family == family && ps[i].Class == class {
			return &ps[i]
		}
	}
	return nil
}

func (f *fixture) granted(family Family, class ActionClass) Level {
	f.t.Helper()
	st, err := f.svc.Granted(f.ctx, family, class)
	if err != nil {
		f.t.Fatalf("granted: %v", err)
	}
	return st.Level
}

// seedL3 grants wraparound_runway x freeze L3 with real evidence.
func (f *fixture) seedL3() {
	f.t.Helper()
	f.seedL2Evidence(FamilyWraparound, 25, 0)
	f.promote(FamilyWraparound, ClassFreeze)
	f.seedL2Recoveries(FamilyWraparound, ClassFreeze, 50)
	if st := f.promote(FamilyWraparound, ClassFreeze); st.Level != L3 {
		f.t.Fatalf("seeded level = %v, want L3", st.Level)
	}
}

func (f *fixture) events(filter EventFilter) []Event {
	f.t.Helper()
	evs, err := f.svc.History(f.ctx, filter)
	if err != nil {
		f.t.Fatalf("history: %v", err)
	}
	return evs
}
