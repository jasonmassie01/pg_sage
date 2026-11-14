package budget

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// market-08b: the cancel counter reads statement-timeout cancels from the
// parsed server log, per role and UTC day, for one database.

type fakeEntries struct {
	mu      sync.Mutex
	entries []logwatch.LogEntry
	evicted int
}

func (f *fakeEntries) Drain() []logwatch.LogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.entries
	f.entries = nil
	return out
}

func (f *fakeEntries) TakeEvicted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.evicted
	f.evicted = 0
	return n
}

func (f *fakeEntries) push(es ...logwatch.LogEntry) {
	f.mu.Lock()
	f.entries = append(f.entries, es...)
	f.mu.Unlock()
}

func timeoutEntry(user, db string, at time.Time) logwatch.LogEntry {
	return logwatch.LogEntry{Timestamp: at, User: user, Database: db, ErrorLevel: "ERROR",
		SQLState: "57014", Message: "canceling statement due to statement timeout"}
}

func alwaysActive() bool { return true }

func countOf(batch CancelBatch, role string, day time.Time) int64 {
	var n int64
	for _, c := range batch.Counts {
		if c.Role == role && c.Day.Equal(day) {
			n += c.Count
		}
	}
	return n
}

// Invalid input: only a statement-timeout cancel counts. A cancel "due to
// user request", an autovacuum cancel, another SQLSTATE and a notice-level
// 57014 do not; a 57014 without English text (lc_messages) does, the safe
// direction. The rule agrees with logwatch's classifier on every case.
func TestIsStatementTimeoutCancel_AgreesWithTheClassifier(t *testing.T) {
	base := timeoutEntry("app", "shop", t0)
	cases := []struct {
		name string
		edit func(*logwatch.LogEntry)
		want bool
	}{
		{"statement timeout", func(*logwatch.LogEntry) {}, true},
		{"user request", func(e *logwatch.LogEntry) {
			e.Message = "canceling statement due to user request"
		}, false},
		{"autovacuum", func(e *logwatch.LogEntry) {
			e.Message = "canceling autovacuum task"
		}, false},
		{"untranslated", func(e *logwatch.LogEntry) {
			e.Message = "Abbruch der Anweisung wegen Zeitüberschreitung"
		}, true},
		{"lock timeout", func(e *logwatch.LogEntry) { e.SQLState = "55P03" }, false},
		{"warning level", func(e *logwatch.LogEntry) { e.ErrorLevel = "WARNING" }, false},
		{"empty sqlstate", func(e *logwatch.LogEntry) { e.SQLState = "" }, false},
	}
	for _, tc := range cases {
		e := base
		tc.edit(&e)
		if got := IsStatementTimeoutCancel(e); got != tc.want {
			t.Errorf("%s: IsStatementTimeoutCancel = %v, want %v", tc.name, got, tc.want)
		}
		cl := logwatch.NewClassifier(logwatch.ClassifierConfig{}, func(string, string,
			...any) {
		})
		sig := cl.Classify(e)
		classified := sig != nil && sig.ID == "log_statement_timeout"
		if classified != tc.want {
			t.Errorf("%s: classifier says log_statement_timeout=%v, the counter %v",
				tc.name, classified, tc.want)
		}
	}
}

// Happy path, nil/empty and invalid input: cancels count per role and UTC
// day; another database, an empty user and a client cancel count nothing.
func TestCancelCounter_CountsPerRoleAndDay(t *testing.T) {
	src := &fakeEntries{}
	c := NewCancelCounter("shop", src, alwaysActive)
	yesterday := t0.Add(-24 * time.Hour)
	userCancel := timeoutEntry("app", "shop", t0)
	userCancel.Message = "canceling statement due to user request"
	src.push(timeoutEntry("app", "shop", t0), timeoutEntry("app", "shop", t0),
		timeoutEntry("app", "shop", yesterday), timeoutEntry("etl", "shop", t0),
		timeoutEntry("app", "billing", t0), timeoutEntry("", "shop", t0), userCancel)
	c.Poll(t0)
	batch := c.Take()
	today := utcDay(t0)
	if got := countOf(batch, "app", today); got != 2 {
		t.Fatalf("app today = %d, want 2 (batch %+v)", got, batch)
	}
	if got := countOf(batch, "app", utcDay(yesterday)); got != 1 {
		t.Fatalf("app yesterday = %d, want 1", got)
	}
	if got := countOf(batch, "etl", today); got != 1 {
		t.Fatalf("etl today = %d, want 1", got)
	}
	if got := countOf(batch, "", today); got != 0 {
		t.Fatalf("an entry without a user counted %d", got)
	}
	if len(batch.LowerBound) != 0 {
		t.Fatalf("lower bound days = %v, want none", batch.LowerBound)
	}
	if again := c.Take(); len(again.Counts) != 0 {
		t.Fatalf("Take did not reset: %+v", again)
	}
}

// Boundary: an evicted entry marks the poll's day a lower bound; Restore
// puts back a batch whose write failed, added to what came since.
func TestCancelCounter_EvictedIsALowerBoundAndRestoreAdds(t *testing.T) {
	src := &fakeEntries{}
	c := NewCancelCounter("shop", src, alwaysActive)
	src.push(timeoutEntry("app", "shop", t0))
	src.evicted = 3
	c.Poll(t0)
	batch := c.Take()
	if len(batch.LowerBound) != 1 || !batch.LowerBound[0].Equal(utcDay(t0)) {
		t.Fatalf("lower bound days = %v, want [%s]", batch.LowerBound, utcDay(t0))
	}
	src.push(timeoutEntry("app", "shop", t0))
	c.Poll(t0)
	c.Restore(batch)
	merged := c.Take()
	if got := countOf(merged, "app", utcDay(t0)); got != 2 {
		t.Fatalf("after restore app = %d, want 2", got)
	}
	if len(merged.LowerBound) != 1 {
		t.Fatalf("restore lost the lower bound: %+v", merged)
	}
}

// Nil/empty: a nil counter has no source, reads nothing and never panics;
// an inactive watcher is no source either.
func TestCancelCounter_NilAndInactive(t *testing.T) {
	var c *CancelCounter
	if c.Active() {
		t.Fatal("a nil counter reports an active log source")
	}
	c.Poll(t0)
	if b := c.Take(); len(b.Counts) != 0 || len(b.LowerBound) != 0 {
		t.Fatalf("nil Take = %+v", b)
	}
	c.Restore(CancelBatch{Counts: []RoleCancels{{Role: "app", Day: t0, Count: 1}}})
	inactive := NewCancelCounter("shop", &fakeEntries{}, func() bool { return false })
	if inactive.Active() {
		t.Fatal("an inactive watcher reports an active log source")
	}
	if NewCancelCounter("shop", nil, alwaysActive).Active() {
		t.Fatal("a counter without a subscription reports an active log source")
	}
}

// Run polls until its context ends.
func TestCancelCounter_RunPollsUntilCancelled(t *testing.T) {
	src := &fakeEntries{}
	c := NewCancelCounter("shop", src, alwaysActive)
	src.push(timeoutEntry("app", "shop", time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	var total int64
	for time.Now().Before(deadline) && total == 0 {
		for _, rc := range c.Take().Counts {
			total += rc.Count
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if total != 1 {
		t.Fatalf("Run counted %d cancels, want 1", total)
	}
}

// csvField quotes one csvlog field.
func csvField(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// csvTimeout is one csvlog line (23 columns) of a statement timeout.
func csvTimeout(user, db string) string {
	rec := make([]string, 23)
	rec[0] = "2026-11-12 10:00:00.000 UTC"
	rec[1], rec[2], rec[3] = user, db, "4242"
	rec[5], rec[11], rec[12] = "sess", "ERROR", "57014"
	rec[13] = "canceling statement due to statement timeout"
	fields := make([]string, len(rec))
	for i, f := range rec {
		fields[i] = csvField(f)
	}
	return strings.Join(fields, ",") + "\n"
}

// Concurrent access: two databases count one role through one watcher,
// polled from their own goroutines, and keep separate per-database counts.
func TestCancelCounter_TwoDatabasesOneWatcher(t *testing.T) {
	dir := t.TempDir()
	var lines strings.Builder
	for range 3 {
		lines.WriteString(csvTimeout("app", "shop"))
	}
	for range 5 {
		lines.WriteString(csvTimeout("app", "billing"))
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.csv"),
		[]byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fw := logwatch.NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 20, MaxLineLenBytes: 65536}, func(string, string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer fw.Stop()
	shop := NewCancelCounter("shop", fw.SubscribeEntries("budget:shop", "shop"), fw.Active)
	billing := NewCancelCounter("billing", fw.SubscribeEntries("budget:billing", "billing"),
		fw.Active)
	totals := map[string]int64{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (totals["shop"] < 3 || totals["billing"] < 5) {
		fw.Drain()
		var wg sync.WaitGroup
		var mu sync.Mutex
		for name, c := range map[string]*CancelCounter{"shop": shop, "billing": billing} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Poll(t0)
				for _, rc := range c.Take().Counts {
					mu.Lock()
					totals[name] += rc.Count
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		time.Sleep(20 * time.Millisecond)
	}
	if totals["shop"] != 3 || totals["billing"] != 5 {
		t.Fatalf("counts = %v, want shop 3 and billing 5", totals)
	}
}
