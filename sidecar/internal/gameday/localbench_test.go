package gameday

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Roadmap 1.1 (2026-10-03): "Run bench locally" runs the PGIncidentBench
// fault programs on a disposable clone (the clone provider, or the local
// development database) and ingests the report as bench evidence marked
// "local run", which counts only for the families it covered. The clone
// is always destroyed; the run never targets a monitored database; one
// run at a time per database.

type benchLedgerCall struct {
	raw []byte
	in  earned.BenchIngest
}

type fakeBenchLedger struct {
	mu    sync.Mutex
	calls []benchLedgerCall
	err   error
}

func (l *fakeBenchLedger) IngestBench(_ context.Context, raw []byte,
	in earned.BenchIngest) (earned.EvalRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, benchLedgerCall{raw: raw, in: in})
	if l.err != nil {
		return earned.EvalRun{}, l.err
	}
	return earned.EvalRun{ID: "eval-1", Origin: in.Origin}, nil
}

func newLocalBench(t *testing.T, p *fakeProvider, f *fakeFaults,
	l *fakeBenchLedger) *LocalBench {
	t.Helper()
	b, err := NewLocalBench(LocalBenchConfig{Database: "orders", Provider: "dle",
		Now: func() time.Time { return time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC) }},
		p, f, l)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLocalBenchRunsOnACloneAndIngestsALocalRun(t *testing.T) {
	p, f, l := &fakeProvider{}, &fakeFaults{report: []byte(`{"report":1}`)},
		&fakeBenchLedger{}
	b := newLocalBench(t, p, f, l)
	run, err := b.Run(context.Background(), []string{"lock_blocking"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != StatusCompleted || run.EvalRunID != "eval-1" || run.CloneID != "clone-1" ||
		run.Database != "orders" || run.Provider != "dle" || run.FinishedAt == nil ||
		strings.Join(run.Families, ",") != "lock_blocking" || run.Error != "" {
		t.Fatalf("run = %+v", run)
	}
	if len(p.created) != 1 || len(p.destroyed) != 1 || !p.specs[0].IncludeData {
		t.Fatalf("clone lifecycle: created %d destroyed %d specs %+v", len(p.created),
			len(p.destroyed), p.specs)
	}
	if len(f.dsns) != 1 || f.dsns[0] != p.created[0].DSN ||
		strings.Join(f.families[0], ",") != "lock_blocking" {
		t.Fatalf("fault programs ran on %v for %v", f.dsns, f.families)
	}
	if len(l.calls) != 1 {
		t.Fatalf("ledger calls = %d", len(l.calls))
	}
	in := l.calls[0].in
	if in.Origin != earned.OriginLocalRun || in.Actor != earned.ActorPgSage ||
		in.Signature != nil || string(l.calls[0].raw) != `{"report":1}` {
		t.Fatalf("ingest = %+v %q", in, l.calls[0].raw)
	}
	if last, ok := b.Last(); !ok || last.ID != run.ID || last.Status != StatusCompleted {
		t.Fatalf("last = %+v %v", last, ok)
	}
	// The clone's DSN carries credentials and is never kept.
	if strings.Contains(run.Error+run.CloneID, "secret") {
		t.Fatal("the clone DSN leaked into the run record")
	}
}

func TestLocalBenchWithoutFamiliesRunsEveryFamily(t *testing.T) {
	f := &fakeFaults{report: []byte(`{}`)}
	b := newLocalBench(t, &fakeProvider{}, f, &fakeBenchLedger{})
	if _, err := b.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.families) != 1 || len(f.families[0]) != 0 {
		t.Fatalf("families = %v, want none (every family)", f.families)
	}
}

func TestLocalBenchFailuresAlwaysDestroyTheClone(t *testing.T) {
	cases := map[string]struct {
		p          *fakeProvider
		f          *fakeFaults
		l          *fakeBenchLedger
		wantStatus string
		wantErr    string
		destroyed  int
	}{
		"fault programs fail": {&fakeProvider{}, &fakeFaults{err: errors.New("inject failed")},
			&fakeBenchLedger{}, StatusFailed, "inject failed", 1},
		"the ledger refuses the report": {&fakeProvider{}, &fakeFaults{report: []byte(`{}`)},
			&fakeBenchLedger{err: earned.ErrBuildMismatch}, StatusFailed, "build", 1},
		"the clone cannot be created": {&fakeProvider{createErr: errors.New("no capacity")},
			&fakeFaults{}, &fakeBenchLedger{}, StatusFailed, "no capacity", 0},
		"the clone cannot be destroyed": {&fakeProvider{destroyErr: errors.New("api down")},
			&fakeFaults{report: []byte(`{}`)}, &fakeBenchLedger{}, StatusDestroyFailed,
			"api down", 1},
	}
	for name, c := range cases {
		b := newLocalBench(t, c.p, c.f, c.l)
		run, err := b.Run(context.Background(), []string{"wal_retention"})
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.wantErr)
		}
		if run.Status != c.wantStatus || !strings.Contains(run.Error, c.wantErr) ||
			run.FinishedAt == nil {
			t.Errorf("%s: run = %+v", name, run)
		}
		if len(c.p.destroyed) != c.destroyed {
			t.Errorf("%s: destroyed %d clones, want %d", name, len(c.p.destroyed),
				c.destroyed)
		}
		if last, ok := b.Last(); !ok || last.Status != c.wantStatus {
			t.Errorf("%s: last = %+v %v", name, last, ok)
		}
	}
}

func TestLocalBenchRefusesUnknownFamilies(t *testing.T) {
	p := &fakeProvider{}
	b := newLocalBench(t, p, &fakeFaults{}, &fakeBenchLedger{})
	for _, families := range [][]string{{"shell"}, {"lock_blocking", "DROP TABLE"}, {""}} {
		if _, err := b.Run(context.Background(), families); !errors.Is(err,
			ErrUnknownFamily) {
			t.Errorf("%v: err = %v, want ErrUnknownFamily", families, err)
		}
		if _, err := b.Start(context.Background(), families); !errors.Is(err,
			ErrUnknownFamily) {
			t.Errorf("start %v: err = %v, want ErrUnknownFamily", families, err)
		}
	}
	if len(p.created) != 0 {
		t.Fatal("a clone was created for an invalid request")
	}
	if _, ok := b.Last(); ok {
		t.Fatal("an invalid request was recorded as a run")
	}
}

func TestLocalBenchRunsOneAtATime(t *testing.T) {
	f := &fakeFaults{report: []byte(`{}`), block: make(chan struct{})}
	l := &fakeBenchLedger{}
	b := newLocalBench(t, &fakeProvider{}, f, l)
	started, err := b.Start(context.Background(), []string{"lock_blocking"})
	if err != nil || started.Status != StatusRunning || started.ID == "" {
		t.Fatalf("start = %+v (%v)", started, err)
	}
	if !b.Running() {
		t.Fatal("not running after Start")
	}
	if _, err := b.Start(context.Background(), nil); !errors.Is(err, ErrRunning) {
		t.Fatalf("second start: err = %v, want ErrRunning", err)
	}
	if _, err := b.Run(context.Background(), nil); !errors.Is(err, ErrRunning) {
		t.Fatalf("run while running: err = %v, want ErrRunning", err)
	}
	if last, ok := b.Last(); !ok || last.ID != started.ID || last.Status != StatusRunning {
		t.Fatalf("last while running = %+v %v", last, ok)
	}
	close(f.block)
	deadline := time.Now().Add(5 * time.Second)
	for b.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	last, ok := b.Last()
	if b.Running() || !ok || last.ID != started.ID || last.Status != StatusCompleted {
		t.Fatalf("after the run: running %v last %+v", b.Running(), last)
	}
	// A background run survives the request that started it.
	ctx, cancel := context.WithCancel(context.Background())
	f2 := &fakeFaults{report: []byte(`{}`), block: make(chan struct{})}
	b2 := newLocalBench(t, &fakeProvider{}, f2, &fakeBenchLedger{})
	if _, err := b2.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(f2.block)
	for b2.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if last, _ := b2.Last(); last.Status != StatusCompleted {
		t.Fatalf("a cancelled request stopped the background run: %+v", last)
	}
}

func TestNewLocalBenchNeedsItsParts(t *testing.T) {
	p, f, l := &fakeProvider{}, &fakeFaults{}, &fakeBenchLedger{}
	cfg := LocalBenchConfig{Database: "orders", Provider: "local"}
	if _, err := NewLocalBench(cfg, nil, f, l); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no provider: err = %v, want ErrNotConfigured", err)
	}
	if _, err := NewLocalBench(LocalBenchConfig{Provider: "local"}, p, f, l); err == nil {
		t.Error("no database accepted")
	}
	if _, err := NewLocalBench(cfg, p, nil, l); err == nil {
		t.Error("no fault programs accepted")
	}
	if _, err := NewLocalBench(cfg, p, f, nil); err == nil {
		t.Error("no ledger accepted")
	}
	b, err := NewLocalBench(cfg, p, f, l)
	if err != nil || b.Provider() != "local" {
		t.Fatalf("bench = %v (%v)", b, err)
	}
}

// The local development target refuses every monitored database, so a
// local bench run can never inject faults into one.
func TestLocalBenchTargetRefusesTheMonitoredDatabase(t *testing.T) {
	monitored := []string{"postgres://app:pw@db.prod:5432/orders",
		"postgres://sage:pw@meta.internal:5432/sage_meta"}
	for _, dsn := range []string{
		"postgres://other:pw@DB.PROD:5432/orders?sslmode=require",
		"postgres://sage:pw@meta.internal:5432/sage_meta",
	} {
		if _, err := NewLocalProvider(dsn, monitored); !errors.Is(err, ErrMonitoredDSN) {
			t.Errorf("%s: err = %v, want ErrMonitoredDSN", dsn, err)
		}
	}
	if _, err := NewLocalProvider("postgres://u:pw@db.prod:5432/scratch", monitored); err != nil {
		t.Fatalf("a disposable database on the same server was refused: %v", err)
	}
}

func TestBenchRegistry(t *testing.T) {
	reg := NewBenchRegistry()
	b := newLocalBench(t, &fakeProvider{}, &fakeFaults{}, &fakeBenchLedger{})
	reg.Register("orders", b)
	reg.Register("", b)
	reg.Register("billing", nil)
	if got, ok := reg.Lookup("orders"); !ok || got != b {
		t.Fatal("orders not registered")
	}
	for _, db := range []string{"", "billing"} {
		if _, ok := reg.Lookup(db); ok {
			t.Errorf("%q registered", db)
		}
	}
	reg.Remove("orders")
	if _, ok := reg.Lookup("orders"); ok {
		t.Fatal("orders still registered after Remove")
	}
}
