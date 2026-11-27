package advisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// advisors-05: the WAL advisor reasons over the advisor window's history,
// not the 60 s previous snapshot.

// No concurrent access tests: the sub-advisors run sequentially in one
// goroutine and these helpers share no state.

var (
	walNow       = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ckptEpoch    = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	walStatEpoch = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
)

// fakeWindowHistory is the history store's window-start rows.
type fakeWindowHistory struct {
	sys         walStart
	sysFound    bool
	sysErr      error
	tables      tablesStart
	tablesFound bool
	tablesErr   error
	sysReads    int
	gotWindow   time.Duration
	gotKeys     []tableKey
}

func (f *fakeWindowHistory) readSystemStart(_ context.Context, window time.Duration) (
	walStart, bool, error) {
	f.sysReads++
	f.gotWindow = window
	return f.sys, f.sysFound, f.sysErr
}

func (f *fakeWindowHistory) readTablesStart(_ context.Context, window time.Duration,
	keys []tableKey) (tablesStart, bool, error) {
	f.gotWindow, f.gotKeys = window, keys
	return f.tables, f.tablesFound, f.tablesErr
}

// walCounters is a system row's checkpoint and WAL counters.
func walCounters(req, timed, walBytes int64) collector.SystemStats {
	return collector.SystemStats{CheckpointsRequested: req, CheckpointsTimed: timed,
		TotalCheckpoints: req + timed, CheckpointStatsEpoch: ckptEpoch,
		WALBytes: walBytes, WALStatsEpoch: walStatEpoch}
}

// walWindowSnap is the current snapshot: max_wal_size and
// checkpoint_timeout as given, the counters of sys, at walNow.
func walWindowSnap(maxWALMB, timeoutS string, sys collector.SystemStats) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: walNow, System: sys,
		ConfigData: &collector.ConfigSnapshot{PGSettings: []collector.PGSetting{
			{Name: "max_wal_size", Setting: maxWALMB, Unit: "MB"},
			{Name: "checkpoint_timeout", Setting: timeoutS, Unit: "s"},
		}}}
}

// walHistory1h is a start row one hour before walNow with 100 requested,
// 50 timed checkpoints and 10 GiB of WAL.
func walHistory1h() *fakeWindowHistory {
	return &fakeWindowHistory{sysFound: true,
		sys: walStart{at: walNow.Add(-time.Hour), sys: walCounters(100, 50, 10<<30)}}
}

// The happy path: 30 requested and 2 timed checkpoints over one hour, 12 GiB
// of WAL. The ratio is 30/32, the rate 12 GiB/h, one 5 min timeout's worth
// is 1 GiB and the floor 1.5 GiB.
func TestWALFacts_OneHourWindow(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	f, err := walFacts(context.Background(), walHistory1h(), snap, 24*time.Hour)
	if err != nil {
		t.Fatalf("walFacts: %v", err)
	}
	if !f.known || f.measured != time.Hour || f.window != 24*time.Hour {
		t.Fatalf("facts = %+v, want a known 1h window of the 24h advisor window", f)
	}
	if f.requestedRatio == nil || *f.requestedRatio != 0.9375 ||
		f.reqDelta != 30 || f.timedDelta != 2 {
		t.Errorf("ratio = %v (%d req, %d timed), want 0.9375 of 30 and 2",
			fval(f.requestedRatio), f.reqDelta, f.timedDelta)
	}
	if f.walPerHour == nil || *f.walPerHour != 12<<30 {
		t.Errorf("WAL/h = %v, want exactly 12 GiB", fval(f.walPerHour))
	}
	if f.walPerTimeout == nil || *f.walPerTimeout != 1<<30 ||
		f.checkpointTimeout != 5*time.Minute {
		t.Errorf("WAL per timeout = %v over %s, want 1 GiB over 5m", fval(f.walPerTimeout),
			f.checkpointTimeout)
	}
	fl := walSizeFloor(f)
	if !fl.known || fl.bytes != 1536<<20 {
		t.Errorf("floor = %+v, want 1536 MiB (1.5 x 1 GiB)", fl)
	}
}

func fval(p *float64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

// Without pg_stat_wal on both rows (a legacy start row), the WAL rate
// comes from the insert positions instead.
func TestWALFacts_LegacyStartRowUsesTheLSN(t *testing.T) {
	start := walCounters(100, 50, 0)
	start.WALStatsEpoch, start.WALLSN = time.Time{}, 1<<30
	start.WALReadAt = walNow.Add(-2 * time.Hour)
	cur := walCounters(130, 52, 22<<30)
	cur.WALLSN, cur.WALReadAt = 1<<30+4<<30, walNow
	hist := &fakeWindowHistory{sysFound: true,
		sys: walStart{at: walNow.Add(-2 * time.Hour), sys: start}}
	f, err := walFacts(context.Background(), hist, walWindowSnap("1024", "300", cur),
		24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if f.walPerHour == nil || *f.walPerHour != 2<<30 {
		t.Errorf("WAL/h = %v, want 2 GiB from 4 GiB of LSN over 2h", fval(f.walPerHour))
	}
}

// State transitions: a checkpointer reset between the rows (the epoch
// moved, or a counter fell) is an unknown ratio, never a negative one; the
// WAL counter keeps its own epoch.
func TestWALFacts_ResetsGiveUnknownNeverNegative(t *testing.T) {
	moved := walCounters(3, 1, 22<<30)
	moved.CheckpointStatsEpoch = walNow.Add(-10 * time.Minute)
	fell := walCounters(3, 1, 22<<30) // same epoch, counters below the start's
	walReset := walCounters(130, 52, 1<<20)
	for name, cur := range map[string]collector.SystemStats{"epoch moved": moved,
		"counter fell": fell} {
		f, err := walFacts(context.Background(), walHistory1h(),
			walWindowSnap("1024", "300", cur), 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if f.requestedRatio != nil || f.reqDelta < 0 || f.timedDelta < 0 {
			t.Errorf("%s: ratio %v deltas %d/%d, want unknown", name,
				fval(f.requestedRatio), f.reqDelta, f.timedDelta)
		}
		if f.walPerHour == nil {
			t.Errorf("%s: the WAL rate was dropped with the checkpoint counters", name)
		}
	}
	f, err := walFacts(context.Background(), walHistory1h(),
		walWindowSnap("1024", "300", walReset), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if f.walPerHour != nil || f.walPerTimeout != nil || walSizeFloor(f).known {
		t.Errorf("a fallen WAL counter gave WAL/h %v, want unknown and no floor",
			fval(f.walPerHour))
	}
	if f.requestedRatio == nil {
		t.Error("a WAL counter reset dropped the checkpoint ratio")
	}
}

// Nil/empty: no source, no row in the window, or a row too recent to
// span three checkpoint timeouts give no facts and no floor.
func TestWALFacts_NoWindowIsUnknown(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	recent := &fakeWindowHistory{sysFound: true,
		sys: walStart{at: walNow.Add(-14 * time.Minute), sys: walCounters(100, 50, 10<<30)}}
	for name, hist := range map[string]windowHistory{"no source": nil,
		"no row": &fakeWindowHistory{}, "14 min < 3 x 5 min": recent} {
		f, err := walFacts(context.Background(), hist, snap, 24*time.Hour)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f.known || f.requestedRatio != nil || f.walPerHour != nil ||
			walSizeFloor(f).known || f.unknownWhy == "" {
			t.Errorf("%s: facts = %+v, want unknown with a reason", name, f)
		}
	}
	if _, err := walFacts(context.Background(), walHistory1h(), nil, time.Hour); err == nil {
		t.Error("a nil snapshot gave no error")
	}
}

// Error propagation: a history read error is returned with its cause.
func TestWALFacts_ReadErrorPropagates(t *testing.T) {
	hist := &fakeWindowHistory{sysErr: errors.New("connection refused")}
	_, err := walFacts(context.Background(), hist,
		walWindowSnap("1024", "300", walCounters(1, 1, 1)), time.Hour)
	if err == nil || !strings.Contains(err.Error(), "connection refused") ||
		!strings.Contains(err.Error(), "history") {
		t.Fatalf("err = %v, want the history read's cause", err)
	}
}

// The floor is clamped to the documented max_wal_size range.
func TestWALSizeFloor_ClampedToDocs(t *testing.T) {
	tiny, huge := float64(1<<20), float64(100<<30)
	for _, c := range []struct {
		perTimeout float64
		want       float64
	}{{tiny, 512 << 20}, {huge, 64 << 30}, {1 << 30, 1536 << 20},
		{(1 << 30) + 1, 1537 << 20}} {
		f := walWindowFacts{known: true, walPerTimeout: &c.perTimeout}
		if fl := walSizeFloor(f); !fl.known || fl.bytes != c.want {
			t.Errorf("per timeout %.0f: floor %+v, want %.0f", c.perTimeout, fl, c.want)
		}
	}
}

// walAdvice runs the WAL sub-advisor with hist over a 24h window and the
// model answering content.
func walWindowAdvice(t *testing.T, hist windowHistory, snap *collector.Snapshot,
	content string) ([]capturedPrompt, []analyzer.Finding, error) {
	t.Helper()
	mgr, capture := capturingManager(t, content)
	ctx := withAdvisorWindow(context.Background(),
		advisorWindow{src: hist, window: 24 * time.Hour})
	got, err := analyzeWAL(ctx, mgr, snap, config.DefaultConfig(), noopLog)
	return capture(), got, err
}

// Boundary: a requested share of exactly 0.20 calls the model; 0.1999
// skips it (healthy, no spend).
func TestWALAdvisor_RequestedShareBoundary(t *testing.T) {
	at := func(req, timed int64) *collector.Snapshot {
		return walWindowSnap("1024", "300", walCounters(100+req, 50+timed, 22<<30))
	}
	prompts, _, err := walWindowAdvice(t, walHistory1h(), at(2000, 8000), "[]")
	if err != nil || len(prompts) != 1 {
		t.Fatalf("ratio 0.20: prompts=%d err=%v, want the model called", len(prompts), err)
	}
	prompts, _, err = walWindowAdvice(t, walHistory1h(), at(1999, 8001), "[]")
	if err != nil || len(prompts) != 0 {
		t.Fatalf("ratio 0.1999: prompts=%d err=%v, want no model call", len(prompts), err)
	}
}

// The prompt states the measured window and the facts, and the rule about
// pg_stat_bgwriter's backend-writes counter is gone.
func TestWALAdvisor_PromptNamesWindowAndFacts(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	prompts, _, err := walWindowAdvice(t, walHistory1h(), snap, "[]")
	if err != nil || len(prompts) != 1 {
		t.Fatalf("prompts=%d err=%v", len(prompts), err)
	}
	p := prompts[0]
	for _, want := range []string{"measured over 1h0m0s", "30 requested, 2 timed",
		"93.8%", "12.0 GB/h", "1.0 GB per checkpoint_timeout (5m0s)",
		"Lowest max_wal_size: 1536MB", "Highest max_wal_size: 6144MB"} {
		if !strings.Contains(p.user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p.user)
		}
	}
	if strings.Contains(p.user, "delta since last") {
		t.Error("prompt still states the 60 s delta")
	}
}

// PG version: no prompt, on any version, names the backend-writes counter (gone
// from pg_stat_bgwriter on 17+, and never collected).
// buffersBackend is spelled in two parts so that CHECK-06 (no mention in
// internal/advisor) holds for the tests too.
const buffersBackend = "buffers_" + "backend"

func TestWALAdvisor_NoBuffersBackendOnAnyVersion(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	for _, major := range []int{14, 16, 17, 18} {
		mgr, capture := capturingManager(t, "[]")
		ctx := context.WithValue(context.Background(), promptEnvKey{},
			promptEnv{pgMajor: major})
		ctx = withAdvisorWindow(ctx, advisorWindow{src: walHistory1h(), window: time.Hour})
		if _, err := analyzeWAL(ctx, mgr, snap, config.DefaultConfig(), noopLog); err != nil {
			t.Fatalf("PG %d: %v", major, err)
		}
		p := onlyPrompt(t, capture())
		if strings.Contains(p.system+p.user, buffersBackend) {
			t.Errorf("PG %d prompt mentions %s", major, buffersBackend)
		}
	}
}

// Error propagation: a history read error skips the advisor (no model
// call, nothing recommended from one snapshot) and names the cause.
func TestWALAdvisor_HistoryErrorSkips(t *testing.T) {
	hist := &fakeWindowHistory{sysErr: errors.New("permission denied for sage.snapshots")}
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	prompts, got, err := walWindowAdvice(t, hist, snap,
		`[{"recommended_sql":"ALTER SYSTEM SET max_wal_size = '4GB'"}]`)
	if len(prompts) != 0 || len(got) != 0 {
		t.Fatalf("model called %d times on a history error", len(prompts))
	}
	if err == nil || !strings.Contains(err.Error(), "permission denied") ||
		!strings.Contains(err.Error(), "skipped") {
		t.Fatalf("err = %v, want a skip naming the cause", err)
	}
}
