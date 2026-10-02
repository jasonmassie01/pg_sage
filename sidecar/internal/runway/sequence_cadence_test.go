package runway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Dogfood lifeos-1: sequences are read on their own, slower cadence
// (their consumption is slow) through the background budget; between
// reads the last reading is evaluated but not re-sampled, a failed read
// waits for the next due time, and the probe's coverage (scan cap,
// truncation) is reported once per change.

type cadenceRunner struct {
	mu         sync.Mutex
	fg, bg     map[probes.ID]int
	seqResult  probes.Result
	seqResults []probes.Result // consumed first, one per background call
}

func newCadenceRunner(seq probes.Result) *cadenceRunner {
	return &cadenceRunner{fg: map[probes.ID]int{}, bg: map[probes.ID]int{},
		seqResult: seq}
}

func (c *cadenceRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fg[id]++
	return probes.Result{ProbeID: id, Status: probes.StatusEmpty, Reason: "no_rows"}
}

func (c *cadenceRunner) RunBackground(_ context.Context, id probes.ID,
	_ probes.Args) probes.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bg[id]++
	if id != probes.SequenceRunwayProbe {
		return probes.Result{ProbeID: id, Status: probes.StatusEmpty}
	}
	if len(c.seqResults) > 0 {
		r := c.seqResults[0]
		c.seqResults = c.seqResults[1:]
		return r
	}
	return c.seqResult
}

func (c *cadenceRunner) counts(id probes.ID) (fg, bg int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fg[id], c.bg[id]
}

func seqResult(truncated bool, total, scanned int64, names ...string) probes.Result {
	res := probes.Result{ProbeID: probes.SequenceRunwayProbe, Status: probes.StatusOK,
		Truncated: truncated, ObservedAt: time.Now()}
	for _, n := range names {
		res.Rows = append(res.Rows, probes.Row{"sequence": n, "data_type": "integer",
			"last_value": int64(900), "min_value": int64(1), "max_value": int64(1000),
			"effective_limit": int64(1000), "fraction_used": 0.9, "cycle": false,
			"sequences_total": total, "sequences_scanned": scanned,
			"sequences_used": int64(len(names)), "sequences_unreadable": int64(0)})
	}
	return res
}

func failedSeq() probes.Result {
	return probes.Result{ProbeID: probes.SequenceRunwayProbe, Status: probes.StatusError,
		Reason: "statement_timeout"}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *logSink) matching(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

func cadenceMonitor(r ProbeRunner, every time.Duration, c *clock, l *logSink) *Monitor {
	opts := testOptions()
	opts.Interval, opts.SequenceInterval = time.Minute, every
	return &Monitor{runner: r, opts: opts, logFn: l.log, now: c.now,
		last: map[seriesKey]lastPoint{}}
}

func TestMonitorRead_SequencesOnTheirOwnCadence(t *testing.T) {
	r := newCadenceRunner(seqResult(false, 3, 3, "public.a_seq"))
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	m := cadenceMonitor(r, 10*time.Minute, c, &logSink{})
	ctx := context.Background()
	snap, errs := m.read(ctx)
	if len(errs) != 0 || !snap.SequencesOK || !snap.SequencesFresh ||
		len(snap.Sequences) != 1 {
		t.Fatalf("first read = %+v (%v), want a fresh reading", snap, errs)
	}
	for _, step := range []time.Duration{time.Minute, 8 * time.Minute} {
		c.add(step)
		snap, errs = m.read(ctx)
		if len(errs) != 0 || !snap.SequencesOK || snap.SequencesFresh ||
			len(snap.Sequences) != 1 {
			t.Fatalf("read before due = %+v (%v), want the cached reading", snap, errs)
		}
	}
	if fg, bg := r.counts(probes.SequenceRunwayProbe); fg != 0 || bg != 1 {
		t.Fatalf("sequence probe ran %d foreground / %d background, want 0 / 1", fg, bg)
	}
	c.add(time.Minute) // exactly 10 minutes after the first read: due
	if snap, _ = m.read(ctx); !snap.SequencesFresh {
		t.Fatal("sequences not re-read when due")
	}
	if _, bg := r.counts(probes.SequenceRunwayProbe); bg != 2 {
		t.Fatalf("background reads = %d, want 2", bg)
	}
	if fg, _ := r.counts(probes.XIDRunwayProbe); fg != 4 {
		t.Fatalf("xid_runway reads = %d, want one per read (4)", fg)
	}
}

// Zero interval keeps the original behaviour: every tick reads.
func TestMonitorRead_ZeroSequenceIntervalReadsEveryTick(t *testing.T) {
	r := newCadenceRunner(seqResult(false, 1, 1, "public.a_seq"))
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	m := cadenceMonitor(r, 0, c, &logSink{})
	for i := 0; i < 3; i++ {
		if snap, _ := m.read(context.Background()); !snap.SequencesFresh {
			t.Fatalf("read %d not fresh", i)
		}
	}
	if _, bg := r.counts(probes.SequenceRunwayProbe); bg != 3 {
		t.Fatalf("background reads = %d, want 3", bg)
	}
}

// Error propagation: a failed read is reported once, not evaluated on
// stale data, and not retried until the next due time.
func TestMonitorRead_FailedSequenceReadWaitsForTheNextDueTime(t *testing.T) {
	r := newCadenceRunner(seqResult(false, 2, 2, "public.b_seq"))
	r.seqResults = []probes.Result{seqResult(false, 2, 2, "public.a_seq"), failedSeq()}
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	m := cadenceMonitor(r, 10*time.Minute, c, &logSink{})
	ctx := context.Background()
	if snap, _ := m.read(ctx); !snap.SequencesOK {
		t.Fatal("first read failed")
	}
	c.add(10 * time.Minute)
	snap, errs := m.read(ctx)
	if snap.SequencesOK || snap.SequencesFresh || len(snap.Sequences) != 0 ||
		len(errs) != 1 || !strings.Contains(errs[0].Error(), "statement_timeout") {
		t.Fatalf("failed read = %+v (%v), want not ok and the timeout named", snap, errs)
	}
	c.add(time.Minute)
	snap, errs = m.read(ctx)
	if snap.SequencesOK || len(errs) != 0 {
		t.Fatalf("read after a failure = %+v (%v), want not ok and no retry", snap, errs)
	}
	if _, bg := r.counts(probes.SequenceRunwayProbe); bg != 2 {
		t.Fatalf("background reads = %d, want no retry before due", bg)
	}
	c.add(9 * time.Minute)
	if snap, _ = m.read(ctx); !snap.SequencesFresh || snap.Sequences[0].Sequence !=
		"public.b_seq" {
		t.Fatalf("read when due again = %+v", snap)
	}
}

// Coverage is logged once per change: a capped scan warns, truncation
// alone is informational, an unchanged coverage logs nothing.
func TestMonitorRead_ReportsCoverageOncePerChange(t *testing.T) {
	r := newCadenceRunner(seqResult(true, 30000, int64(probes.SequenceScanCap), "public.a"))
	r.seqResults = []probes.Result{
		seqResult(true, 30000, int64(probes.SequenceScanCap), "public.a"),
		seqResult(true, 30000, int64(probes.SequenceScanCap), "public.a"),
		seqResult(true, 12038, 12038, "public.a")}
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	l := &logSink{}
	m := cadenceMonitor(r, time.Minute, c, l)
	for i := 0; i < 3; i++ {
		m.read(context.Background())
		c.add(time.Minute)
	}
	capped := l.matching("scan cap")
	if len(capped) != 1 || !strings.HasPrefix(capped[0], "WARN") ||
		!strings.Contains(capped[0], "30000") {
		t.Fatalf("scan cap logs = %v, want one WARN naming the total", capped)
	}
	trunc := l.matching("nearest their limit")
	if len(trunc) != 1 || !strings.HasPrefix(trunc[0], "INFO") ||
		!strings.Contains(trunc[0], "12038") {
		t.Fatalf("truncation logs = %v, want one INFO for the new coverage", trunc)
	}
}

// Concurrent reads (Tick and Sample) share one cache: inside the interval
// the probe runs once.
func TestMonitorRead_ConcurrentReadsShareTheCache(t *testing.T) {
	r := newCadenceRunner(seqResult(false, 1, 1, "public.a_seq"))
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	m := cadenceMonitor(r, 10*time.Minute, c, &logSink{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, _ := m.read(context.Background())
			if !snap.SequencesOK || len(snap.Sequences) != 1 {
				t.Errorf("concurrent read = %+v", snap)
			}
		}()
	}
	wg.Wait()
	if _, bg := r.counts(probes.SequenceRunwayProbe); bg != 1 {
		t.Fatalf("background reads = %d, want 1", bg)
	}
}

// A cached (not fresh) reading is evaluated but never sampled again: a
// re-sampled stale value would flatten the trend.
func TestBuildSamples_OnlyFreshSequencesAreSampled(t *testing.T) {
	s := fullSnapshot()
	s.SequencesFresh = true
	if _, ok := byKey(BuildSamples(s, Options{}))["sequence/public.a_seq"]; !ok {
		t.Fatal("a fresh sequence reading was not sampled")
	}
	s.SequencesFresh = false
	for k := range byKey(BuildSamples(s, Options{})) {
		if strings.HasPrefix(k, "sequence/") {
			t.Fatalf("cached sequence reading sampled as %s", k)
		}
	}
}

func TestNewMonitor_ValidatesTheSequenceInterval(t *testing.T) {
	opts := testOptions()
	opts.Interval, opts.Retention = time.Minute, 48*time.Hour
	for _, d := range []time.Duration{-time.Second, 30 * time.Second} {
		o := opts
		o.SequenceInterval = d
		if err := o.validate(); err == nil || !strings.Contains(err.Error(), "sequence") {
			t.Errorf("sequence interval %s: err = %v, want rejected", d, err)
		}
	}
	for _, d := range []time.Duration{0, time.Minute, 10 * time.Minute} {
		o := opts
		o.SequenceInterval = d
		if err := o.validate(); err != nil {
			t.Errorf("sequence interval %s rejected: %v", d, err)
		}
	}
}
