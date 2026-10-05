package selfconfig

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Integration (real Postgres): the engine derives every key per database,
// records it in the derivation ledger (value, previous value, cited
// evidence, bounds, rule and version, time), keeps new values in shadow
// for the soak, promotes them only when the comparison is not worse, and
// writes in-force values into the runtime config.

func reconcile(t *testing.T, e *Engine, in Input) map[string]Result {
	t.Helper()
	results, err := e.Reconcile(t.Context(), in)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	out := map[string]Result{}
	for _, r := range results {
		out[r.Key] = r
	}
	return out
}

func TestStartupRecordsEveryCandidateInShadow(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	got := reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(),
		Evidence: heavyEvidence(), Phase: PhaseStartup})
	want := map[string]float64{
		"collector.interval_seconds": 180, "safety.query_timeout_ms": 1200,
		"sre.runways.sequence_interval_seconds": 1500, "sre.detectors.temp_file_mb": 8192,
		"sre.detectors.lwlock_waiters": 20,
	}
	if len(got) != len(want) {
		t.Fatalf("results %v", got)
	}
	for key, v := range want {
		r := got[key]
		if r.Status != StatusShadow || r.Shadow == nil || *r.Shadow != v || r.Applied {
			t.Errorf("%s: %+v, want shadow %v not applied", key, r, v)
		}
	}
	// Nothing applied: the runtime keeps the defaults during the soak.
	if cfg.Collector.IntervalSeconds != 60 || cfg.Safety.QueryTimeoutMs != 500 ||
		cfg.SRE.Detectors.LWLockWaiters != 8 {
		t.Fatalf("shadow values were applied: %+v %+v", cfg.Collector, cfg.Safety)
	}
	entries, err := e.Store.History(t.Context(), "safety.query_timeout_ms", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("history %v %v", entries, err)
	}
	h := entries[0]
	if h.Event != EventShadow || h.Value == nil || *h.Value != 1200 || h.Previous == nil ||
		*h.Previous != 500 || h.Rule == "" || h.RuleVersion < 1 || h.Reason == "" ||
		h.Actor != "pg_sage" || !h.At.Equal(clk.Now()) {
		t.Fatalf("ledger entry %+v", h)
	}
	cited := map[string]float64{}
	for _, c := range h.Evidence {
		cited[c.Name] = c.Value
	}
	if cited["catalog_scan_ms"] != 300 || cited["relations"] != 250000 {
		t.Fatalf("evidence %+v", h.Evidence)
	}
	if h.Bounds.Min != 500 || h.Bounds.Max != 5000 || h.Bounds.Default != 500 {
		t.Fatalf("bounds %+v", h.Bounds)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	in := Input{Cfg: defaults(), OperatorSet: noOperator(), Evidence: heavyEvidence(),
		Phase: PhaseStartup}
	reconcile(t, e, in)
	before := ledgerCount(t, pool, "", "")
	clk.Advance(time.Hour)
	in.Phase = PhaseLive
	reconcile(t, e, in)
	reconcile(t, e, in)
	if after := ledgerCount(t, pool, "", ""); after != before {
		t.Fatalf("unchanged evidence grew the ledger %d -> %d", before, after)
	}
}

// collector.interval_seconds is adopted live: after the soak, with three
// samples showing pg_sage over its 1% budget, it is promoted and applied.
func TestLiveKeyPromotedAfterSoakAndApplied(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	ev := Evidence{CollectorCycleMs: Known(1800), Relations: Known(9000)}
	reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseStartup})
	for i := 0; i < 3; i++ {
		clk.Advance(time.Hour)
		r := reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
			Phase: PhaseLive})["collector.interval_seconds"]
		if r.Applied || cfg.Collector.IntervalSeconds != 60 {
			t.Fatalf("applied during the soak: %+v", r)
		}
	}
	clk.Advance(21 * time.Hour) // 24 h after the shadow was recorded
	r := reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseLive})["collector.interval_seconds"]
	if r.Status != StatusDerived || r.Value != 180 || !r.Applied {
		t.Fatalf("not promoted: %+v", r)
	}
	if cfg.Collector.IntervalSeconds != 180 {
		t.Fatalf("runtime interval %d, want 180", cfg.Collector.IntervalSeconds)
	}
	if ledgerCount(t, pool, "collector.interval_seconds", "promoted") != 1 {
		t.Fatal("promotion not in the ledger")
	}
}

func TestShortSoakFromConfig(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	cfg.SelfConfig.SoakHours = 1
	ev := Evidence{MaxConnections: Known(1000)}
	reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseStartup})
	clk.Advance(59 * time.Minute)
	if r := reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseStartup})["sre.detectors.lwlock_waiters"]; r.Status != StatusShadow {
		t.Fatalf("promoted before a 1 h soak: %+v", r)
	}
	clk.Advance(time.Minute)
	r := reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseStartup})["sre.detectors.lwlock_waiters"]
	if r.Status != StatusDerived || cfg.SRE.Detectors.LWLockWaiters != 20 {
		t.Fatalf("not promoted after a 1 h soak: %+v (runtime %d)", r,
			cfg.SRE.Detectors.LWLockWaiters)
	}
}

// A restart-bound key soaks and is promoted while running, is reported
// pending, and takes effect at the next start.
func TestRestartKeyPendingThenAppliedAtStartup(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	ev := Evidence{CatalogScanMs: Known(300), Relations: Known(250000)}
	reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseStartup})
	for i := 0; i < 24; i++ {
		clk.Advance(time.Hour)
		reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
			Phase: PhaseLive})
	}
	st, err := e.Store.Get(t.Context(), "safety.query_timeout_ms")
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != 500 || st.Pending == nil || *st.Pending != 1200 ||
		cfg.Safety.QueryTimeoutMs != 500 {
		t.Fatalf("live promotion: state %+v runtime %d", st, cfg.Safety.QueryTimeoutMs)
	}
	fresh := defaults() // the next process start
	r := reconcile(t, e, Input{Cfg: fresh, OperatorSet: noOperator(), Evidence: ev,
		Phase: PhaseStartup})["safety.query_timeout_ms"]
	if !r.Applied || r.Value != 1200 || r.Pending != nil || fresh.Safety.QueryTimeoutMs != 1200 {
		t.Fatalf("startup: %+v runtime %d", r, fresh.Safety.QueryTimeoutMs)
	}
	if ledgerCount(t, pool, "safety.query_timeout_ms", "applied") != 1 {
		t.Fatal("the restart application is not in the ledger")
	}
}

func TestOperatorSetKeyIsNeverWritten(t *testing.T) {
	pool, _ := testPool(t)
	e, _ := newTestEngine(pool)
	cfg := defaults()
	cfg.Collector.IntervalSeconds = 45 // the operator's value
	got := reconcile(t, e, Input{Cfg: cfg, Evidence: heavyEvidence(), Phase: PhaseStartup,
		OperatorSet: map[string]bool{"collector.interval_seconds": true}})
	r := got["collector.interval_seconds"]
	if r.Status != StatusOperator || r.Value != 45 || r.Applied || r.Shadow != nil {
		t.Fatalf("operator key: %+v", r)
	}
	if cfg.Collector.IntervalSeconds != 45 {
		t.Fatalf("the operator's value was overwritten: %d", cfg.Collector.IntervalSeconds)
	}
	if ledgerCount(t, pool, "collector.interval_seconds", "operator_set") != 1 ||
		ledgerCount(t, pool, "collector.interval_seconds", "shadow") != 0 {
		t.Fatal("operator key ledger wrong")
	}
	if got["safety.query_timeout_ms"].Status != StatusShadow {
		t.Fatal("other keys stopped deriving")
	}
}

func TestUnknownOperatorSetFailsClosed(t *testing.T) {
	pool, _ := testPool(t)
	e, _ := newTestEngine(pool)
	cfg := defaults()
	_, err := e.Reconcile(t.Context(), Input{Cfg: cfg, Evidence: heavyEvidence(),
		Phase: PhaseStartup})
	if !errors.Is(err, ErrOperatorSetUnknown) {
		t.Fatalf("nil operator set: %v, want ErrOperatorSetUnknown", err)
	}
	if ledgerCount(t, pool, "", "") != 0 {
		t.Fatal("ledger written without knowing the operator's keys")
	}
	if _, err := e.Reconcile(t.Context(), Input{OperatorSet: noOperator(),
		Phase: PhaseStartup}); err == nil {
		t.Fatal("nil runtime config accepted")
	}
	if _, err := NewEngine(NewStore(nil)).Reconcile(t.Context(), Input{Cfg: cfg,
		OperatorSet: noOperator(), Phase: PhaseStartup}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil pool: %v", err)
	}
	bad := Input{Cfg: cfg, OperatorSet: noOperator(), Phase: "sometimes"}
	if _, err := e.Reconcile(t.Context(), bad); err == nil {
		t.Fatal("unknown phase accepted")
	}
}

// Hot reload: a reload that rebuilds the runtime config from the file
// drops derived values; the next pass restores in-force values (live and
// restart-bound alike) without a new ledger entry.
func TestHotReloadDropsAreRestored(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	cfg.SelfConfig.SoakHours = 1
	ev := heavyEvidence()
	in := Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev, Phase: PhaseStartup}
	reconcile(t, e, in)
	clk.Advance(time.Hour)
	reconcile(t, e, in) // stability keys promote at startup (lwlock_waiters)
	if cfg.SRE.Detectors.LWLockWaiters != 20 {
		t.Fatalf("lwlock_waiters %d after promotion", cfg.SRE.Detectors.LWLockWaiters)
	}
	ledger := ledgerCount(t, pool, "", "")
	reloaded := defaults()
	reloaded.SelfConfig.SoakHours = 1
	*cfg = *reloaded // what a whole-config reload does
	in.Phase = PhaseLive
	got := reconcile(t, e, in)
	if cfg.SRE.Detectors.LWLockWaiters != 20 || !got["sre.detectors.lwlock_waiters"].Applied {
		t.Fatalf("restart-bound startup value not restored after the reload: %d %+v",
			cfg.SRE.Detectors.LWLockWaiters, got["sre.detectors.lwlock_waiters"])
	}
	if after := ledgerCount(t, pool, "", ""); after != ledger {
		t.Fatalf("restoring after a reload wrote %d ledger entries", after-ledger)
	}
}

// An operator who adds a key to the YAML while pg_sage runs takes it over:
// derivation stops writing it immediately; the runtime value changes when
// the config system applies it (here: the reload), never by derivation.
func TestOperatorTakesOverAtRuntime(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	cfg.SelfConfig.SoakHours = 1
	in := Input{Cfg: cfg, OperatorSet: noOperator(),
		Evidence: Evidence{MaxConnections: Known(1000)}, Phase: PhaseStartup}
	reconcile(t, e, in)
	clk.Advance(time.Hour)
	reconcile(t, e, in)
	clk.Advance(time.Hour)
	in.Phase = PhaseLive
	in.OperatorSet = map[string]bool{"sre.detectors.lwlock_waiters": true}
	operatorCfg := defaults()
	operatorCfg.SRE.Detectors.LWLockWaiters = 12
	in.Operator = operatorCfg
	r := reconcile(t, e, in)["sre.detectors.lwlock_waiters"]
	if r.Status != StatusOperator || r.Value != 20 || r.Pending == nil || *r.Pending != 12 ||
		r.Applied || cfg.SRE.Detectors.LWLockWaiters != 20 {
		t.Fatalf("operator takeover: %+v runtime %d", r, cfg.SRE.Detectors.LWLockWaiters)
	}
}

// A derived value that would make the whole configuration invalid is held,
// never applied.
func TestInvalidResultIsHeld(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	broken := Rules()
	for i := range broken {
		if broken[i].Key == "sre.detectors.lwlock_waiters" {
			broken[i].HardMax = 20000 // past the key's validated range (1-10000)
			broken[i].Derive = func(Evidence, *config.Config) Derivation {
				return Derivation{OK: true, Value: 15000, Reason: "test"}
			}
		}
	}
	e.Rules = broken
	cfg := defaults()
	cfg.SelfConfig.SoakHours = 1
	in := Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: Evidence{},
		Phase: PhaseStartup}
	reconcile(t, e, in)
	clk.Advance(time.Hour)
	r := reconcile(t, e, in)["sre.detectors.lwlock_waiters"]
	if r.Applied || cfg.SRE.Detectors.LWLockWaiters != 8 {
		t.Fatalf("an invalid derived value was applied: %+v runtime %d", r,
			cfg.SRE.Detectors.LWLockWaiters)
	}
	entries, err := e.Store.History(t.Context(), "sre.detectors.lwlock_waiters", 10)
	if err != nil || len(entries) == 0 || entries[0].Event != EventHeld ||
		!strings.Contains(entries[0].Reason, "invalid") {
		t.Fatalf("hold not recorded: %+v %v", entries, err)
	}
}

func TestSummaryNamesEveryKey(t *testing.T) {
	results := []Result{
		{Key: "collector.interval_seconds", Status: StatusDerived, Value: 180},
		{Key: "safety.query_timeout_ms", Status: StatusShadow, Value: 500, Shadow: f(1200)},
		{Key: "sre.detectors.lwlock_waiters", Status: StatusOperator, Value: 12},
		{Key: "sre.detectors.temp_file_mb", Status: StatusPinned, Value: 2048},
		{Key: "sre.runways.sequence_interval_seconds", Status: StatusDefault, Value: 600,
			Pending: f(900)},
	}
	s := Summary(results)
	for _, want := range []string{"collector.interval_seconds=180 (derived)",
		"safety.query_timeout_ms=500 (shadow 1200)", "sre.detectors.lwlock_waiters=12 (operator)",
		"sre.detectors.temp_file_mb=2048 (pinned)",
		"sre.runways.sequence_interval_seconds=600 (default, 900 pending restart)"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q omits %q", s, want)
		}
	}
	if Summary(nil) != "none" {
		t.Errorf("empty summary %q", Summary(nil))
	}
}

// Regression (found by the full suite, TestFleetCollectionStatusUsesManagedCollector):
// a runtime value that neither the operator set in a file nor derivation
// promoted (set programmatically, by a fleet default, by a test) is never
// overwritten with the product default.
func TestUnderivedRuntimeValueIsNeverOverwritten(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	cfg.Collector.IntervalSeconds = 1
	cfg.Safety.QueryTimeoutMs = 750
	in := Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: Evidence{},
		Phase: PhaseStartup}
	got := reconcile(t, e, in)
	clk.Advance(time.Hour)
	in.Phase = PhaseLive
	reconcile(t, e, in)
	if cfg.Collector.IntervalSeconds != 1 || cfg.Safety.QueryTimeoutMs != 750 {
		t.Fatalf("runtime values overwritten: %d %d", cfg.Collector.IntervalSeconds,
			cfg.Safety.QueryTimeoutMs)
	}
	if r := got["collector.interval_seconds"]; r.Applied || r.Value != 1 ||
		r.Status != StatusDefault {
		t.Fatalf("collector result %+v", r)
	}
}
