package logwatch

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-07b: the classifier attaches the parsed participants to the deadlock
// signal; statement text only in redacted mode.

const literalDetail = "Process 4242 waits for ShareLock on transaction 745; " +
	"blocked by process 4243.\n" +
	"Process 4243 waits for ShareLock on transaction 744; blocked by process 4242.\n" +
	"Process 4242: UPDATE users SET email = 'alice@example.com' WHERE card = 4111111111111111\n" +
	"Process 4243: UPDATE users SET email = 'bob@example.org' WHERE id = 1"

func deadlockWith(pid int, at time.Time, detail, context string) LogEntry {
	e := deadlockAt(pid, at)
	e.Detail, e.Context = detail, context
	return e
}

func participantsOf(t *testing.T, s *rca.Signal) []rca.DeadlockParticipant {
	t.Helper()
	if s == nil {
		t.Fatal("no signal")
	}
	parts, _ := s.Metrics[rca.MetricDeadlockParticipants].([]rca.DeadlockParticipant)
	return parts
}

// Happy path, fingerprint mode (the default): two participants with
// fingerprints, no statement text, no literal anywhere in the metric.
func TestClassifier_DeadlockParticipantsFingerprintMode(t *testing.T) {
	c := defaultClassifier()
	sig := c.Classify(deadlockWith(4242, occBase, literalDetail, accountsContext))
	parts := participantsOf(t, sig)
	if len(parts) != 2 || parts[0].PID != 4242 || parts[1].BlockedBy != 4242 {
		t.Fatalf("participants = %+v, want 4242 and 4243", parts)
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{`"statement"`, "alice", "4111111111111111"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("participants %s hold %q in fingerprint mode", raw, leak)
		}
	}
	if parts[0].Fingerprint == "" || parts[0].Fingerprint == parts[1].Fingerprint {
		t.Errorf("fingerprints %q, %q; want two distinct", parts[0].Fingerprint,
			parts[1].Fingerprint)
	}
	if _, ok := sig.Metrics[rca.MetricParticipantsUnparsed]; ok {
		t.Errorf("participants_unparsed set with nothing unparsed: %v", sig.Metrics)
	}
	if got := sig.Metrics["affected_pids"]; !reflect.DeepEqual(got, []int{4242, 4243}) {
		t.Errorf("affected_pids = %v, want [4242 4243] (rca-07 unchanged)", got)
	}
}

// State: redacted mode keeps the text, still without literals; an unknown
// mode keeps none (fails closed).
func TestClassifier_DeadlockParticipantsRedactedMode(t *testing.T) {
	c := NewClassifier(ClassifierConfig{DedupWindowS: 60,
		DeadlockStatementText: "redacted"}, nopLog)
	parts := participantsOf(t, c.Classify(deadlockWith(4242, occBase, literalDetail, "")))
	if len(parts) != 2 || parts[0].Statement == "" {
		t.Fatalf("participants = %+v, want statement text", parts)
	}
	if !strings.HasPrefix(parts[0].Statement, "UPDATE users SET email = '?'") ||
		strings.Contains(parts[0].Statement, "alice") ||
		strings.Contains(parts[0].Statement, "4111111111111111") {
		t.Errorf("statement = %q", parts[0].Statement)
	}
	odd := NewClassifier(ClassifierConfig{DedupWindowS: 60,
		DeadlockStatementText: "raw"}, nopLog)
	for _, p := range participantsOf(t, odd.Classify(deadlockWith(1, occBase, literalDetail, ""))) {
		if p.Statement != "" {
			t.Errorf("unknown mode kept statement %q", p.Statement)
		}
	}
}

// Invalid input: a German DETAIL gives no participants, counts the lines
// unparsed, and still emits the deadlock signal with the logging PID.
func TestClassifier_DeadlockGermanDetailCountsUnparsed(t *testing.T) {
	de := "Prozess 4242 wartet auf ShareLock-Sperre auf Transaktion 745; " +
		"blockiert von Prozess 4243.\nProzess 4242: UPDATE t SET v = 1"
	sig := defaultClassifier().Classify(deadlockWith(4242, occBase, de, ""))
	if sig == nil || sig.ID != "log_deadlock_detected" {
		t.Fatalf("signal = %+v, want the deadlock signal", sig)
	}
	if parts := participantsOf(t, sig); parts != nil {
		t.Errorf("participants = %+v, want none", parts)
	}
	if got := sig.Metrics[rca.MetricParticipantsUnparsed]; got != 2 {
		t.Errorf("participants_unparsed = %v, want 2", got)
	}
}

// Nil/empty: no DETAIL gives the PID-only signal of today.
func TestClassifier_DeadlockEmptyDetailIsPIDOnly(t *testing.T) {
	sig := defaultClassifier().Classify(deadlockAt(77, occBase))
	if _, ok := sig.Metrics[rca.MetricDeadlockParticipants]; ok {
		t.Errorf("participants metric set without a DETAIL: %v", sig.Metrics)
	}
	if got := sig.Metrics["affected_pids"]; !reflect.DeepEqual(got, []int{77}) {
		t.Errorf("affected_pids = %v, want [77]", got)
	}
}

// Only the deadlock signal carries participants, even when another
// signal's DETAIL happens to read like one.
func TestClassifier_ParticipantsOnlyOnDeadlock(t *testing.T) {
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.Message = "canceling statement due to statement timeout"
		e.Detail = "Process 1 waits for ShareLock on transaction 9; blocked by process 2."
	})
	sig := defaultClassifier().Classify(e)
	if sig == nil {
		t.Fatal("no timeout signal")
	}
	if _, ok := sig.Metrics[rca.MetricDeadlockParticipants]; ok {
		t.Errorf("timeout signal carries participants: %v", sig.Metrics)
	}
}

// State transition: the follow-up of a window carries the newest
// deadlock's participants, and the next window starts from its own.
func TestClassifier_FollowUpCarriesNewestParticipants(t *testing.T) {
	c := defaultClassifier()
	newer := "Process 5001 waits for ExclusiveLock on relation 16384 of database 5; " +
		"blocked by process 5002.\nProcess 5002: LOCK TABLE t"
	c.Classify(deadlockWith(4242, occBase, literalDetail, ""))
	if s := c.Classify(deadlockWith(5001, occBase.Add(time.Second), newer, "")); s != nil {
		t.Fatalf("second deadlock in the window emitted at once: %+v", s)
	}
	ups := c.FollowUps()
	if len(ups) != 1 {
		t.Fatalf("follow-ups = %d, want 1", len(ups))
	}
	parts := participantsOf(t, ups[0])
	if len(parts) != 2 || parts[0].PID != 5001 || parts[1].PID != 5002 {
		t.Errorf("follow-up participants = %+v, want the newest (5001, 5002)", parts)
	}
	if got := ups[0].Metrics["occurrence_count"]; got != 2 {
		t.Errorf("occurrence_count = %v, want 2", got)
	}
	// A deadlock without a parsable DETAIL keeps the window's last set.
	c.Classify(deadlockAt(6000, occBase.Add(2*time.Second)))
	parts = participantsOf(t, c.FollowUps()[0])
	if len(parts) != 2 || parts[0].PID != 5001 {
		t.Errorf("participants after a DETAIL-less repeat = %+v, want 5001's", parts)
	}
}

// Concurrency: Classify and the drain (FollowUps, CleanExpiredDedup) race
// on deadlock lines; every signal carries a whole participant set and
// the windows' totals add up to the lines classified.
func TestClassifier_DeadlockClassifyAndDrainRace(t *testing.T) {
	c := defaultClassifier()
	var mu sync.Mutex
	var sigs []*rca.Signal
	collect := func(s ...*rca.Signal) {
		mu.Lock()
		defer mu.Unlock()
		sigs = append(sigs, s...)
	}
	var wg sync.WaitGroup
	for w := 0; w < raceWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < raceLinesPerWorker; i++ {
				at := occBase.Add(time.Duration(w*raceLinesPerWorker+i) * time.Millisecond)
				if s := c.Classify(deadlockWith(4242, at, literalDetail, "")); s != nil {
					collect(s)
				}
			}
		}(w)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			collect(c.FollowUps()...)
			c.CleanExpiredDedup()
		}
	}()
	wg.Wait()
	<-done
	c.CleanExpiredDedup()
	collect(c.FollowUps()...)
	assertWholeSetsAndTotals(t, sigs)
}

const raceWorkers, raceLinesPerWorker = 4, 50

// assertWholeSetsAndTotals checks every signal's participant set and that
// the windows' totals (each window's largest count) add up to every line.
func assertWholeSetsAndTotals(t *testing.T, sigs []*rca.Signal) {
	t.Helper()
	totals := map[any]int{}
	for _, s := range sigs {
		if parts := participantsOf(t, s); len(parts) != 2 || parts[1].PID != 4243 {
			t.Fatalf("signal participants = %+v, want a whole set", parts)
		}
		n, _ := s.Metrics["occurrence_count"].(int)
		totals[s.Metrics["first_seen"]] = max(totals[s.Metrics["first_seen"]], n)
	}
	sum := 0
	for _, n := range totals {
		sum += n
	}
	if sum != raceWorkers*raceLinesPerWorker {
		t.Errorf("windows total %d occurrences, want %d", sum, raceWorkers*raceLinesPerWorker)
	}
}

// State: the unread DETAIL lines of every deadlock in a window add up on
// the window's follow-up (two German reports of 2 lines each give 4).
func TestClassifier_UnparsedLinesSumOverWindow(t *testing.T) {
	c := defaultClassifier()
	de := "Prozess 4242 wartet auf ShareLock-Sperre auf Transaktion 745; " +
		"blockiert von Prozess 4243.\nProzess 4242: UPDATE t SET v = 1"
	c.Classify(deadlockWith(4242, occBase, de, ""))
	if s := c.Classify(deadlockWith(4243, occBase.Add(time.Second), de, "")); s != nil {
		t.Fatalf("second deadlock in the window emitted at once: %+v", s)
	}
	ups := c.FollowUps()
	if len(ups) != 1 {
		t.Fatalf("follow-ups = %d, want 1", len(ups))
	}
	if got := ups[0].Metrics[rca.MetricParticipantsUnparsed]; got != 4 {
		t.Errorf("participants_unparsed = %v, want 4 (2 lines in each of 2 reports)", got)
	}
}
