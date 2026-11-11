package logwatch

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-07: the classifier keeps counting the repeats it deduplicates and
// reports them in one follow-up signal per dedup entry at drain.

var occBase = time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

func deadlockAt(pid int, at time.Time) LogEntry {
	return baseEntry(func(e *LogEntry) {
		e.Timestamp = at
		e.SQLState = "40P01"
		e.PID = pid
		e.Message = "deadlock detected"
	})
}

func followUpsWithID(sigs []*rca.Signal, id string) []*rca.Signal {
	var out []*rca.Signal
	for _, s := range sigs {
		if s.ID == id {
			out = append(out, s)
		}
	}
	return out
}

// Happy path: three deadlocks from PIDs 101/102/103 in one window give one
// immediate signal plus one follow-up carrying the count and the PIDs.
func TestClassifier_FollowUpCountsDeduplicatedRepeats(t *testing.T) {
	c := defaultClassifier()
	var immediate []*rca.Signal
	for i, pid := range []int{101, 102, 103} {
		if s := c.Classify(deadlockAt(pid, occBase.Add(time.Duration(i)*time.Second))); s != nil {
			immediate = append(immediate, s)
		}
	}
	if len(immediate) != 1 {
		t.Fatalf("immediate signals = %d, want 1", len(immediate))
	}
	if got := immediate[0].Metrics["occurrence_count"]; got != 1 {
		t.Errorf("immediate occurrence_count = %v, want 1", got)
	}
	if got := immediate[0].Metrics["affected_pids"]; !reflect.DeepEqual(got, []int{101}) {
		t.Errorf("immediate affected_pids = %v, want [101]", got)
	}
	ups := c.FollowUps()
	if len(ups) != 1 {
		t.Fatalf("follow-ups = %d, want 1: %+v", len(ups), ups)
	}
	up := ups[0]
	if up.ID != "log_deadlock_detected" || up.Severity != "critical" {
		t.Errorf("follow-up id/severity = %s/%s", up.ID, up.Severity)
	}
	if got := up.Metrics["occurrence_count"]; got != 3 {
		t.Errorf("occurrence_count = %v, want 3", got)
	}
	if got := up.Metrics["affected_pids"]; !reflect.DeepEqual(got, []int{101, 102, 103}) {
		t.Errorf("affected_pids = %v, want [101 102 103]", got)
	}
	if got := up.Metrics["first_seen"]; got != occBase.Format(time.RFC3339Nano) {
		t.Errorf("first_seen = %v, want %s", got, occBase.Format(time.RFC3339Nano))
	}
	last := occBase.Add(2 * time.Second)
	if got := up.Metrics["last_seen"]; got != last.Format(time.RFC3339Nano) {
		t.Errorf("last_seen = %v, want %s", got, last.Format(time.RFC3339Nano))
	}
	if !up.FiredAt.Equal(last) {
		t.Errorf("follow-up FiredAt = %v, want the last occurrence %v", up.FiredAt, last)
	}
	if got := up.Metrics["database"]; got != "testdb" {
		t.Errorf("follow-up database = %v, want testdb", got)
	}
}

// State transition: everything counted was reported, so nothing repeats
// until a new occurrence, whose follow-up carries the window's total.
func TestClassifier_FollowUpReportsOnlyNewRepeats(t *testing.T) {
	c := defaultClassifier()
	for i, pid := range []int{101, 102, 103} {
		c.Classify(deadlockAt(pid, occBase.Add(time.Duration(i)*time.Second)))
	}
	if ups := c.FollowUps(); len(ups) != 1 || ups[0].Metrics["occurrence_count"] != 3 {
		t.Fatalf("first FollowUps = %+v, want one with count 3", ups)
	}
	if again := c.FollowUps(); len(again) != 0 {
		t.Errorf("second FollowUps = %d signals, want 0", len(again))
	}
	c.Classify(deadlockAt(104, occBase.Add(3*time.Second)))
	ups := c.FollowUps()
	if len(ups) != 1 || ups[0].Metrics["occurrence_count"] != 4 {
		t.Fatalf("follow-up after a fourth repeat = %+v, want count 4", ups)
	}
}

// Nil/empty: a single occurrence, or no occurrence, needs no follow-up.
func TestClassifier_NoFollowUpWithoutRepeats(t *testing.T) {
	c := defaultClassifier()
	if got := c.FollowUps(); len(got) != 0 {
		t.Fatalf("empty classifier follow-ups = %v", got)
	}
	if s := c.Classify(deadlockAt(101, occBase)); s == nil {
		t.Fatal("first deadlock emitted no signal")
	}
	if got := c.FollowUps(); len(got) != 0 {
		t.Fatalf("single occurrence follow-ups = %v, want none", got)
	}
}

// Boundary: an event exactly at the dedup-window edge starts a new entry,
// and the closed entry's unreported repeats are still reported.
func TestClassifier_WindowEdgeStartsNewEntry(t *testing.T) {
	c := NewClassifier(ClassifierConfig{DedupWindowS: 60, MaxLinesPerCycle: 100}, nopLog)
	if c.Classify(deadlockAt(101, occBase)) == nil {
		t.Fatal("first deadlock suppressed")
	}
	if c.Classify(deadlockAt(102, occBase.Add(59*time.Second))) != nil {
		t.Fatal("repeat inside the window was not deduplicated")
	}
	edge := c.Classify(deadlockAt(103, occBase.Add(60*time.Second)))
	if edge == nil {
		t.Fatal("event exactly at the window edge was deduplicated")
	}
	if edge.Metrics["occurrence_count"] != 1 {
		t.Errorf("new entry occurrence_count = %v, want 1", edge.Metrics["occurrence_count"])
	}
	ups := c.FollowUps()
	if len(ups) != 1 {
		t.Fatalf("follow-ups = %d, want 1 for the closed window", len(ups))
	}
	if ups[0].Metrics["occurrence_count"] != 2 ||
		!reflect.DeepEqual(ups[0].Metrics["affected_pids"], []int{101, 102}) {
		t.Errorf("closed window follow-up = %v", ups[0].Metrics)
	}
}

// Boundary: affected_pids holds at most 20 PIDs; the count keeps all 21.
func TestClassifier_AffectedPIDsCappedAtTwenty(t *testing.T) {
	c := defaultClassifier()
	for pid := 1; pid <= 21; pid++ {
		c.Classify(deadlockAt(pid, occBase.Add(time.Duration(pid)*time.Millisecond)))
	}
	ups := c.FollowUps()
	if len(ups) != 1 {
		t.Fatalf("follow-ups = %d, want 1", len(ups))
	}
	pids, _ := ups[0].Metrics["affected_pids"].([]int)
	if len(pids) != MaxAffectedPIDs || MaxAffectedPIDs != 20 {
		t.Fatalf("affected_pids = %v (%d), want 20", pids, len(pids))
	}
	for i, pid := range pids {
		if pid != i+1 {
			t.Fatalf("affected_pids = %v, want 1..20 in order", pids)
		}
	}
	if ups[0].Metrics["occurrence_count"] != 21 {
		t.Errorf("occurrence_count = %v, want 21", ups[0].Metrics["occurrence_count"])
	}
}

// The deadlock DETAIL names both participants; their PIDs are evidence.
// Statements in the DETAIL are not copied into affected_pids.
func TestClassifier_DeadlockParticipantsFromDetail(t *testing.T) {
	c := defaultClassifier()
	e := deadlockAt(5678, occBase)
	e.Detail = "Process 5678 waits for ShareLock on transaction 743; blocked by " +
		"process 5679.\nProcess 5679 waits for ShareLock on transaction 742; " +
		"blocked by process 5678.\nProcess 5678: UPDATE t SET v = 1 WHERE id = 1;"
	s := c.Classify(e)
	if s == nil {
		t.Fatal("deadlock emitted no signal")
	}
	if got := s.Metrics["affected_pids"]; !reflect.DeepEqual(got, []int{5678, 5679}) {
		t.Errorf("affected_pids = %v, want [5678 5679]", got)
	}
}

// Invalid input: a cancel by pg_cancel_backend is not a timeout incident.
func TestClassifier_UserRequestCancelIsInfoSignal(t *testing.T) {
	c := defaultClassifier()
	s := c.Classify(baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.Message = "canceling statement due to user request"
	}))
	if s == nil {
		t.Fatal("user-request cancel emitted no signal")
	}
	if s.ID != "log_statement_cancelled" || s.Severity != "info" {
		t.Fatalf("signal = %s/%s, want log_statement_cancelled/info", s.ID, s.Severity)
	}
	timeout := c.Classify(baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.Message = "canceling statement due to statement timeout"
	}))
	if timeout == nil || timeout.ID != "log_statement_timeout" {
		t.Fatalf("statement timeout = %+v, want log_statement_timeout", timeout)
	}
	if _, ok := timeout.Metrics["message_unclassified"]; ok {
		t.Error("an English timeout was marked message_unclassified")
	}
}

// Invalid input: with lc_messages German the text cannot be split, so the
// signal stays a timeout and says the message was not classified.
func TestClassifier_NonEnglishCancelStaysTimeout(t *testing.T) {
	c := defaultClassifier()
	s := c.Classify(baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.Message = "storniere Anfrage wegen Benutzeraufforderung"
	}))
	if s == nil || s.ID != "log_statement_timeout" {
		t.Fatalf("signal = %+v, want log_statement_timeout", s)
	}
	if s.Metrics["message_unclassified"] != true {
		t.Errorf("message_unclassified = %v, want true", s.Metrics["message_unclassified"])
	}
}

// An autovacuum worker cancelled by a lock request logs 57014 at ERROR; it
// is the autovacuum-cancel signal, not a statement timeout.
func TestClassifier_AutovacuumCancelAtErrorIsNotTimeout(t *testing.T) {
	c := defaultClassifier()
	s := c.Classify(baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.Message = "canceling autovacuum task"
	}))
	if s == nil || s.ID != "log_autovacuum_cancel" {
		t.Fatalf("signal = %+v, want log_autovacuum_cancel", s)
	}
}

// Expired entries are cleaned only after their repeats are reported.
func TestClassifier_CleanKeepsUnreportedRepeats(t *testing.T) {
	c := NewClassifier(ClassifierConfig{DedupWindowS: 1, MaxLinesPerCycle: 100}, nopLog)
	old := time.Now().Add(-time.Hour)
	c.Classify(deadlockAt(101, old))
	c.Classify(deadlockAt(102, old.Add(100*time.Millisecond)))
	c.CleanExpiredDedup()
	ups := c.FollowUps()
	if len(ups) != 1 || ups[0].Metrics["occurrence_count"] != 2 {
		t.Fatalf("follow-ups after clean = %+v, want one with count 2", ups)
	}
	c.mu.Lock()
	n := len(c.dedupBuf)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("expired entries kept = %d, want 0", n)
	}
}

// Concurrent access: Classify racing FollowUps and CleanExpiredDedup on one
// classifier loses no occurrence and reports the first one once.
func TestClassifier_ClassifyRacesDrain(t *testing.T) {
	c := NewClassifier(ClassifierConfig{DedupWindowS: 3600, MaxLinesPerCycle: 0}, nopLog)
	const writers, perWriter = 8, 250
	at := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []*rca.Signal
	record := func(sigs ...*rca.Signal) {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range sigs {
			if s != nil {
				all = append(all, s)
			}
		}
	}
	stop := startDrainer(c, record)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				record(c.Classify(deadlockAt(w*1000+i, at)))
			}
		}(w)
	}
	wg.Wait()
	stop()
	record(c.FollowUps()...)
	maxCount, firsts := 0, 0
	for _, s := range all {
		n, _ := s.Metrics["occurrence_count"].(int)
		if n > maxCount {
			maxCount = n
		}
		if n == 1 {
			firsts++
		}
	}
	if maxCount != writers*perWriter {
		t.Errorf("highest occurrence_count = %d, want %d", maxCount, writers*perWriter)
	}
	if firsts != 1 {
		t.Errorf("signals with count 1 = %d, want exactly the immediate one", firsts)
	}
}

// startDrainer drains c's follow-ups and cleans its dedup buffer in a loop
// until the returned stop is called; stop waits for the loop to end.
func startDrainer(c *Classifier, record func(...*rca.Signal)) (stop func()) {
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		for {
			select {
			case <-done:
				return
			default:
				record(c.FollowUps()...)
				c.CleanExpiredDedup()
			}
		}
	}()
	return func() { close(done); <-ended }
}
