package selfconfig

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Integration (real Postgres): pin current / unpin state transitions and
// their ledger rows, and concurrent writers.

func seedDerived(t *testing.T, e *Engine, clk *clock) {
	t.Helper()
	cfg := defaults()
	cfg.SelfConfig.SoakHours = 1
	in := Input{Cfg: cfg, OperatorSet: noOperator(),
		Evidence: Evidence{MaxConnections: Known(1000)}, Phase: PhaseStartup}
	reconcile(t, e, in)
	clk.Advance(time.Hour)
	if r := reconcile(t, e, in)["sre.detectors.lwlock_waiters"]; r.Status != StatusDerived {
		t.Fatalf("seed: %+v", r)
	}
}

func TestPinCurrentAndUnpin(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	seedDerived(t, e, clk)
	ctx := t.Context()
	pinned, err := e.Store.Pin(ctx, "sre.detectors.lwlock_waiters", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Status != StatusPinned || pinned.Pinned == nil || *pinned.Pinned != 20 ||
		pinned.PinnedBy != "admin@example.com" || pinned.PinnedAt.IsZero() {
		t.Fatalf("pinned %+v", pinned)
	}
	entries, _ := e.Store.History(ctx, "sre.detectors.lwlock_waiters", 1)
	if len(entries) != 1 || entries[0].Event != EventPinned || *entries[0].Value != 20 ||
		entries[0].Actor != "admin@example.com" {
		t.Fatalf("pin ledger %+v", entries)
	}
	// Pinning again is a no-op: no second ledger row.
	before := ledgerCount(t, pool, "sre.detectors.lwlock_waiters", "pinned")
	if again, err := e.Store.Pin(ctx, "sre.detectors.lwlock_waiters", "x"); err != nil ||
		again.PinnedBy != "admin@example.com" {
		t.Fatalf("re-pin %+v %v", again, err)
	}
	if ledgerCount(t, pool, "sre.detectors.lwlock_waiters", "pinned") != before {
		t.Fatal("re-pinning wrote a ledger row")
	}
	// Evidence now says something else: the pin holds.
	clk.Advance(3 * time.Hour)
	cfg := defaults()
	r := reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(),
		Evidence: Evidence{MaxConnections: Known(3000)}, Phase: PhaseStartup})
	if got := r["sre.detectors.lwlock_waiters"]; got.Status != StatusPinned || got.Value != 20 ||
		cfg.SRE.Detectors.LWLockWaiters != 20 || got.Shadow != nil {
		t.Fatalf("pin did not hold against new evidence: %+v", got)
	}
	unpinned, err := e.Store.Unpin(ctx, "sre.detectors.lwlock_waiters", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if unpinned.Pinned != nil || unpinned.Status == StatusPinned {
		t.Fatalf("unpinned %+v", unpinned)
	}
	if ledgerCount(t, pool, "sre.detectors.lwlock_waiters", "unpinned") != 1 {
		t.Fatal("unpin not in the ledger")
	}
	// Derivation resumes: the new evidence becomes a shadow candidate.
	clk.Advance(time.Hour)
	r = reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(),
		Evidence: Evidence{MaxConnections: Known(3000)}, Phase: PhaseStartup})
	if got := r["sre.detectors.lwlock_waiters"]; got.Status != StatusShadow ||
		got.Shadow == nil || *got.Shadow != 60 || got.Value != 20 {
		t.Fatalf("after unpin %+v", got)
	}
}

func TestPinAndUnpinErrors(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	ctx := t.Context()
	if _, err := e.Store.Pin(ctx, "trust.level", "a"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("pin a non-derived key: %v", err)
	}
	if _, err := e.Store.Pin(ctx, "sre.detectors.lwlock_waiters", "a"); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("pin before any derivation: %v", err)
	}
	seedDerived(t, e, clk)
	if _, err := e.Store.Unpin(ctx, "sre.detectors.lwlock_waiters", "a"); !errors.Is(err,
		ErrNotPinned) {
		t.Fatalf("unpin of an unpinned key: %v", err)
	}
	if _, err := e.Store.Pin(ctx, "sre.detectors.lwlock_waiters", ""); err == nil {
		t.Fatal("pin without an actor accepted")
	}
	// An operator-set key is pinned by the configuration: neither pin nor
	// unpin applies.
	reconcile(t, e, Input{Cfg: defaults(), Evidence: Evidence{}, Phase: PhaseStartup,
		OperatorSet: map[string]bool{"collector.interval_seconds": true}})
	if _, err := e.Store.Pin(ctx, "collector.interval_seconds", "a"); !errors.Is(err,
		ErrOperatorSet) {
		t.Fatalf("pin an operator key: %v", err)
	}
	if _, err := e.Store.Unpin(ctx, "collector.interval_seconds", "a"); !errors.Is(err,
		ErrOperatorSet) {
		t.Fatalf("unpin an operator key: %v", err)
	}
	if _, err := NewStore(nil).Pin(ctx, "collector.interval_seconds", "a"); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("nil pool: %v", err)
	}
}

func TestPinClearsAPendingShadow(t *testing.T) {
	pool, _ := testPool(t)
	e, _ := newTestEngine(pool)
	reconcile(t, e, Input{Cfg: defaults(), OperatorSet: noOperator(),
		Evidence: Evidence{MaxConnections: Known(1000)}, Phase: PhaseStartup})
	st, err := e.Store.Pin(t.Context(), "sre.detectors.lwlock_waiters", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if st.Shadow != nil || *st.Pinned != 8 || st.Status != StatusPinned {
		t.Fatalf("pin of the current (default) value with a shadow pending: %+v", st)
	}
}

// Concurrent reconcilers (e.g. a startup pass racing a live pass) record
// one shadow per candidate; a pin racing a reconcile always ends pinned.
func TestConcurrentWriters(t *testing.T) {
	pool, _ := testPool(t)
	e, _ := newTestEngine(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Reconcile(t.Context(), Input{Cfg: defaults(),
				OperatorSet: noOperator(), Evidence: heavyEvidence(), Phase: PhaseLive})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent reconcile: %v", err)
		}
	}
	if n := ledgerCount(t, pool, "safety.query_timeout_ms", "shadow"); n != 1 {
		t.Fatalf("%d shadow rows for one candidate", n)
	}

	var pinErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, pinErr = e.Store.Pin(t.Context(), "safety.query_timeout_ms", "admin")
	}()
	go func() {
		defer wg.Done()
		_, _ = e.Reconcile(t.Context(), Input{Cfg: defaults(), OperatorSet: noOperator(),
			Evidence: heavyEvidence(), Phase: PhaseLive})
	}()
	wg.Wait()
	if pinErr != nil {
		t.Fatal(pinErr)
	}
	r := reconcile(t, e, Input{Cfg: defaults(), OperatorSet: noOperator(),
		Evidence: heavyEvidence(), Phase: PhaseLive})
	if r["safety.query_timeout_ms"].Status != StatusPinned {
		t.Fatalf("the pin was lost to a concurrent reconcile: %+v", r["safety.query_timeout_ms"])
	}
}

func TestListCarriesHistoryNewestFirst(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	seedDerived(t, e, clk)
	settings, err := e.Store.List(t.Context(), 5)
	if err != nil {
		t.Fatal(err)
	}
	var lw *Setting
	for i := range settings {
		if settings[i].Key == "sre.detectors.lwlock_waiters" {
			lw = &settings[i]
		}
	}
	if lw == nil || len(lw.History) != 2 || lw.History[0].Event != EventPromoted ||
		lw.History[1].Event != EventShadow {
		t.Fatalf("history %+v", lw)
	}
	if len(settings) != 5 {
		t.Fatalf("%d settings listed, want one per rule", len(settings))
	}
	if _, err := e.Store.History(t.Context(), "sre.detectors.lwlock_waiters", 0); err == nil {
		t.Fatal("history limit 0 accepted")
	}
}
