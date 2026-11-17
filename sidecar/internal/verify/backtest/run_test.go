package backtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

func TestConfig_ValidateNamesTheSetting(t *testing.T) {
	base := Config{Database: "db", Days: 90, Placebos: 300, MaxRuntime: time.Minute}
	cases := []struct {
		mutate func(*Config)
		want   string
	}{
		{func(c *Config) { c.Days = 0 }, "--days"},
		{func(c *Config) { c.Days = -1 }, "--days"},
		{func(c *Config) { c.Placebos = 0 }, "--placebos"},
		{func(c *Config) { c.Database = "" }, "--database"},
		{func(c *Config) { c.MaxRuntime = 0 }, "verify.backtest_max_runtime"},
	}
	for _, tc := range cases {
		c := base
		tc.mutate(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%+v) = %v, want it to name %s", c, err, tc.want)
		}
		if _, err := Run(context.Background(), noBlocks(t), c); err == nil {
			t.Errorf("Run accepted %+v", c)
		}
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	d := Config{Database: "db"}.Defaults()
	if d.Days != DefaultDays || d.Placebos != DefaultPlacebos ||
		d.MaxRuntime != DefaultMaxRuntime || d.Cycle != time.Minute || d.Now == nil {
		t.Fatalf("Defaults = %+v", d)
	}
}

// A database with 3 days of history gives "not enough history: 3 of 28
// days", never a rate of 0.
func TestRun_NotEnoughHistory(t *testing.T) {
	m, err := FixtureAt(FixtureSeasonalWeek, FixtureStart, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig("young", 1)
	cfg.Now = func() time.Time { return FixtureStart.Add(3 * day) }
	r, err := Run(context.Background(), m, cfg)
	if !errors.Is(err, ErrNotEnoughHistory) || err.Error() != "not enough history: 3 of 28 days" {
		t.Fatalf("Run = %+v, %v; want not enough history: 3 of 28 days", r, err)
	}
	if r != (Result{}) {
		t.Fatalf("a refused run returned %+v", r)
	}
	empty := NewMemory("empty", FixtureStart, nil, nil, nil, nil)
	if _, err := Run(context.Background(), empty, cfg); err == nil ||
		err.Error() != "not enough history: 0 of 28 days" {
		t.Fatalf("empty history: %v", err)
	}
	// 90 days of history with --days 10 replays 10 days: not enough.
	cfg.Days = 10
	cfg.Now = func() time.Time { return fixtureEnd }
	full, _ := Fixture(FixtureSeasonalWeek)
	if _, err := Run(context.Background(), full, cfg); err == nil ||
		err.Error() != "not enough history: 10 of 28 days" {
		t.Fatalf("--days 10: %v", err)
	}
}

func TestRun_NoStatementsDrawsNothing(t *testing.T) {
	m, _ := FixtureAt(FixtureSeasonalWeek, FixtureStart, 30, time.Hour)
	bare := NewMemory("bare", FixtureStart, nil, nil, map[int64][]Interval{
		101: m.Intervals(101)}, nil)
	cfg := fixtureConfig("bare", 1)
	cfg.Now = func() time.Time { return FixtureStart.Add(30 * day) }
	if _, err := Run(context.Background(), bare, cfg); err == nil ||
		!strings.Contains(err.Error(), "no placebo could be drawn") {
		t.Fatalf("Run without statements = %v", err)
	}
}

// failing fails every read after the first after reads.
type failing struct {
	*Memory
	after int64
	reads atomic.Int64
}

var errSnapshot = errors.New("snapshot read failed: connection reset")

func (f *failing) Window(ctx context.Context, ids []int64, from, to time.Time,
	width time.Duration) (verify.CounterWindow, error) {
	if f.reads.Add(1) > f.after {
		return verify.CounterWindow{}, errSnapshot
	}
	return f.Memory.Window(ctx, ids, from, to, width)
}

// A snapshot read failure mid-run returns an error naming the window and
// no result.
func TestRun_ReadFailureNamesTheWindow(t *testing.T) {
	m, _ := Fixture(FixtureSeasonalWeek)
	r, err := Run(context.Background(), &failing{Memory: m, after: 40},
		fixtureConfig("fails", 1))
	if !errors.Is(err, errSnapshot) {
		t.Fatalf("Run = %v, want the read failure", err)
	}
	msg := err.Error()
	for _, want := range []string{"placebo ", " of 300 at ", "look ", ".."} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if r != (Result{}) {
		t.Fatalf("a failed run returned %+v", r)
	}
}

// A run past its runtime stops with ErrRuntimeExceeded and no result: a
// partial rate is never published.
func TestRun_StopsAtMaxRuntime(t *testing.T) {
	m, _ := Fixture(FixtureSeasonalWeek)
	cfg := fixtureConfig("slow", 1)
	cfg.MaxRuntime = 10 * time.Minute
	var ticks atomic.Int64
	cfg.Now = func() time.Time {
		return fixtureEnd.Add(time.Duration(ticks.Add(1)) * time.Minute)
	}
	r, err := Run(context.Background(), m, cfg)
	if !errors.Is(err, ErrRuntimeExceeded) || !strings.Contains(err.Error(),
		"no result stored") {
		t.Fatalf("Run = %v, want ErrRuntimeExceeded", err)
	}
	if r != (Result{}) {
		t.Fatalf("a stopped run returned %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, m, fixtureConfig("cancelled", 1)); err == nil {
		t.Fatal("a cancelled run returned a result")
	}
}

// Memory is safe for concurrent readers: parallel runs agree.
func TestRun_ConcurrentRunsAgree(t *testing.T) {
	m, _ := FixtureAt(FixtureTrafficShift, FixtureStart, 30, 15*time.Minute)
	cfg := fixtureConfig("parallel", 7)
	cfg.Placebos = 40
	cfg.Now = func() time.Time { return FixtureStart.Add(30 * day) }
	var wg sync.WaitGroup
	results := make([]Result, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Run(context.Background(), m, cfg)
		}()
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != results[0] {
			t.Fatalf("run %d = %+v %v, run 0 = %+v", i, results[i], errs[i], results[0])
		}
	}
	if results[0].V2.Placebos != 40 {
		t.Fatalf("placebos %d, want 40", results[0].V2.Placebos)
	}
}

func TestMemory_WindowMatchesTheCycles(t *testing.T) {
	m, _ := FixtureAt(FixtureDeployBreaks, FixtureStart, 8, 15*time.Minute)
	from := FixtureStart.Add(day)
	to := from.Add(6 * time.Hour)
	w, err := m.Window(context.Background(), []int64{101}, from, to, time.Hour)
	if err != nil || w.EpochBreak {
		t.Fatalf("Window = %+v, %v", w, err)
	}
	var calls int64
	for _, iv := range m.Intervals(101) {
		if !iv.To.Before(from) && iv.To.Before(to) {
			calls += iv.Calls
		}
	}
	var got int64
	for _, b := range w.Series[101] {
		got += b.Calls
	}
	if got != calls || len(w.Series[101]) != 6 {
		t.Fatalf("window calls %d in %d buckets, want %d in 6", got, len(w.Series[101]),
			calls)
	}
	tue := FixtureStart.Add(day + 14*time.Hour) // the Tuesday break
	broken, _ := m.Window(context.Background(), nil, tue.Add(-time.Hour), tue.Add(time.Hour),
		0)
	if !broken.EpochBreak {
		t.Fatal("a window over the Tuesday deploy has no epoch break")
	}
	if _, err := m.Window(context.Background(), nil, to, from, 0); err == nil {
		t.Fatal("an inverted window was accepted")
	}
	s, err := m.Seasonal(context.Background(), []int64{101}, tue, tue.Add(time.Hour), 1)
	if err != nil || len(s[101]) != 0 {
		t.Fatalf("seasonal a week before the first Tuesday = %+v %v, want none", s, err)
	}
}
