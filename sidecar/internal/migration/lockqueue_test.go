package migration

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// migration-05: an in-flight DDL is re-evaluated on every poll against
// the latest lock-chain observation, and the queue behind it escalates.

type fakeQueueSource struct {
	mu     sync.Mutex
	chains []analyzer.LockChain
	at     time.Time
	fresh  bool
	nudges int
}

func (f *fakeQueueSource) Fresh() ([]analyzer.LockChain, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chains, f.at, f.fresh
}

func (f *fakeQueueSource) Nudge() {
	f.mu.Lock()
	f.nudges++
	f.mu.Unlock()
}

type captureSink struct {
	mu       sync.Mutex
	findings []MigrationSafetyFinding
}

func (s *captureSink) UpsertMigrationSafetyFinding(
	_ context.Context, f MigrationSafetyFinding,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findings = append(s.findings, f)
	return int64(len(s.findings)), nil
}

func (s *captureSink) last(t *testing.T) MigrationSafetyFinding {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.findings) == 0 {
		t.Fatal("no finding was persisted")
	}
	return s.findings[len(s.findings)-1]
}

func (s *captureSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.findings)
}

var (
	ddlStart   = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ddlQueryAt = ddlStart.Add(time.Minute)
)

func waitingDDL(pid int) activeDDL {
	return activeDDL{PID: pid, Query: "ALTER TABLE public.orders ADD CONSTRAINT " +
		"c CHECK (status <> 's3cret') NOT VALID", WaitEventType: "Lock",
		BackendStart: ddlStart, QueryStart: ddlQueryAt}
}

func warningIncident() *rca.Incident {
	return &rca.Incident{Severity: "warning", Source: "schema_advisor",
		RootCause: "Dangerous DDL: ddl_constraint_not_valid", DatabaseName: "db1",
		SignalIDs: []string{"ddl_constraint_not_valid"}, Confidence: 0.6,
		AffectedObjects: []string{"public.orders"}}
}

// queueDetector is a detector over a fixed activity list and analysis.
func queueDetector(
	ddls *[]activeDDL, inc func() *rca.Incident, src LockQueueSource,
) (*Detector, *captureSink, *int) {
	cfg := &config.MigrationConfig{Enabled: true, ActivityPolling: true}
	sink := &captureSink{}
	analyses := 0
	advisor := NewAdvisor(nil, cfg, 170000, "db1", func(string, string, ...any) {}, nil)
	d := NewDetector(nil, advisor, cfg, func(string, string, ...any) {}).
		WithFindingSink(sink)
	if src != nil {
		d = d.WithLockQueue(src)
	}
	d.activity = func(context.Context) ([]activeDDL, error) { return *ddls, nil }
	d.analyze = func(context.Context, activeDDL) (*rca.Incident, *DDLRisk, error) {
		analyses++
		return inc(), nil, nil
	}
	return d, sink, &analyses
}

func queueChain(ddlPID, waiters int) analyzer.LockChain {
	blocked := []int{ddlPID}
	for i := 0; i < waiters; i++ {
		blocked = append(blocked, 900+i)
	}
	return analyzer.LockChain{RootBlockerPID: 100, BlockedPIDs: blocked,
		TotalBlocked: len(blocked), DirectWaiters: map[int]int{100: 1, ddlPID: waiters},
		OldestWaitSince: ddlQueryAt}
}

var querySHARe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Boundary: 0 and 2 waiters keep the original severity; 3 escalates.
func TestLockQueue_EscalationBoundary(t *testing.T) {
	for _, tc := range []struct {
		waiters  int
		severity string
	}{{0, "warning"}, {2, "warning"}, {3, "critical"}, {5, "critical"}} {
		src := &fakeQueueSource{fresh: true, at: time.Now(),
			chains: []analyzer.LockChain{queueChain(200, tc.waiters)}}
		ddls := []activeDDL{waitingDDL(200)}
		d, sink, _ := queueDetector(&ddls, warningIncident, src)
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("waiters=%d: PollOnce: %v", tc.waiters, err)
		}
		f := sink.last(t)
		if f.Severity != tc.severity {
			t.Fatalf("waiters=%d: severity %q, want %q", tc.waiters, f.Severity, tc.severity)
		}
		if f.Detail["lock_queue_waiters"] != tc.waiters {
			t.Fatalf("waiters=%d: detail waiters = %v", tc.waiters,
				f.Detail["lock_queue_waiters"])
		}
		escalated := f.Detail["lock_queue_escalated"] == true
		if escalated != (tc.waiters >= 3) {
			t.Fatalf("waiters=%d: escalated = %v", tc.waiters, escalated)
		}
		if strings.Contains(f.RecommendedSQL, "pg_cancel_backend") {
			t.Fatalf("waiters=%d: cancel in executable SQL %q", tc.waiters, f.RecommendedSQL)
		}
		if escalated {
			assertCancelGuidance(t, f, 200)
		}
	}
}

func assertCancelGuidance(t *testing.T, f MigrationSafetyFinding, pid int) {
	t.Helper()
	d := f.Detail
	if d["lock_queue_root_pid"] != 100 || d["pid"] != pid ||
		d["backend_start"] != ddlStart || d["query_start"] != ddlQueryAt ||
		d["state"] != "active" || d["lock_queue_oldest_wait"] != ddlQueryAt {
		t.Fatalf("backend identity/evidence = %v", d)
	}
	if sha, _ := d["query_sha"].(string); !querySHARe.MatchString(sha) {
		t.Fatalf("query_sha = %v, want 16 hex characters", d["query_sha"])
	}
	want := "SELECT pg_cancel_backend(200)"
	if d["cancel_guidance_sql"] != want {
		t.Fatalf("cancel_guidance_sql = %v, want %q", d["cancel_guidance_sql"], want)
	}
	for _, s := range []string{want, "SET lock_timeout = '5s'", "pg_signal_backend",
		"approval"} {
		if !strings.Contains(f.Recommendation, s) {
			t.Fatalf("recommendation %q lacks %q", f.Recommendation, s)
		}
	}
	if q, _ := d["query"].(string); strings.Contains(q, "s3cret") || q == "" {
		t.Fatalf("query evidence keeps literals: %q", q)
	}
}

// Nil/empty: no lock-queue source, or a stale observation, leaves the
// severity alone and says the queue was not observed.
func TestLockQueue_NotObserved(t *testing.T) {
	for name, src := range map[string]LockQueueSource{
		"no source": nil,
		"stale": &fakeQueueSource{fresh: false,
			chains: []analyzer.LockChain{queueChain(200, 5)}},
	} {
		ddls := []activeDDL{waitingDDL(200)}
		d, sink, _ := queueDetector(&ddls, warningIncident, src)
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("%s: PollOnce: %v", name, err)
		}
		f := sink.last(t)
		if f.Severity != "warning" || f.Detail["lock_queue"] != "lock queue not observed" {
			t.Fatalf("%s: severity %q lock_queue %v", name, f.Severity, f.Detail["lock_queue"])
		}
		if _, ok := f.Detail["lock_queue_escalated"]; ok {
			t.Fatalf("%s: escalated without an observation", name)
		}
	}
}

// A DDL waiting on a lock nudges the fast path; one running does not.
func TestLockQueue_NudgesOnlyWhenWaitingOnLock(t *testing.T) {
	src := &fakeQueueSource{}
	running := waitingDDL(200)
	running.WaitEventType = "IO"
	ddls := []activeDDL{running}
	d, _, _ := queueDetector(&ddls, warningIncident, src)
	_, _ = d.PollOnce(context.Background())
	if src.nudges != 0 {
		t.Fatalf("running DDL nudged %d times", src.nudges)
	}
	ddls = []activeDDL{waitingDDL(200)}
	_, _ = d.PollOnce(context.Background())
	_, _ = d.PollOnce(context.Background())
	if src.nudges != 2 {
		t.Fatalf("waiting DDL nudged %d times over 2 polls, want 2", src.nudges)
	}
}

// A DDL the advisor scores low (plain ADD COLUMN) still gets a finding
// once the queue behind it crosses the threshold.
func TestLockQueue_QuietAdvisorStillEscalates(t *testing.T) {
	quiet := func() *rca.Incident { return nil }
	src := &fakeQueueSource{fresh: true, at: time.Now(),
		chains: []analyzer.LockChain{queueChain(200, 2)}}
	ddl := waitingDDL(200)
	ddl.Query = "ALTER TABLE public.orders ADD COLUMN note text"
	ddls := []activeDDL{ddl}
	d, sink, _ := queueDetector(&ddls, quiet, src)
	_, _ = d.PollOnce(context.Background())
	if sink.count() != 0 {
		t.Fatalf("2 waiters behind a quiet DDL persisted %d findings", sink.count())
	}
	src.chains = []analyzer.LockChain{queueChain(200, 3)}
	_, _ = d.PollOnce(context.Background())
	f := sink.last(t)
	if f.RuleID != "ddl_lock_queue" || f.Severity != "critical" ||
		f.Detail["lock_queue_waiters"] != 3 || f.DatabaseName != "db1" {
		t.Fatalf("queue finding = %+v", f)
	}
	if !strings.HasPrefix(f.ObjectIdentifier, "public.orders:ddl_lock_queue:") {
		t.Fatalf("object identifier %q, want the DDL's table", f.ObjectIdentifier)
	}
	assertCancelGuidance(t, f, 200)
}

// State transitions: the DDL is analysed once but re-upserted on every
// poll (bumping last_seen), escalates when the queue builds and returns
// to its own severity when the queue drains. A new backend on a reused
// pid is analysed again.
func TestDetector_ReUpsertsEveryPollAndTracksQueue(t *testing.T) {
	src := &fakeQueueSource{fresh: true, at: time.Now(),
		chains: []analyzer.LockChain{queueChain(200, 0)}}
	ddls := []activeDDL{waitingDDL(200)}
	d, sink, analyses := queueDetector(&ddls, warningIncident, src)
	severities := []string{}
	for _, waiters := range []int{0, 4, 0} {
		src.chains = []analyzer.LockChain{queueChain(200, waiters)}
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		severities = append(severities, sink.last(t).Severity)
	}
	if *analyses != 1 || sink.count() != 3 {
		t.Fatalf("analyses=%d upserts=%d, want 1 analysis and 3 upserts",
			*analyses, sink.count())
	}
	if strings.Join(severities, ",") != "warning,critical,warning" {
		t.Fatalf("severities over the queue = %v", severities)
	}
	ids := map[string]bool{}
	for _, f := range sink.findings {
		ids[f.ObjectIdentifier] = true
	}
	if len(ids) != 1 {
		t.Fatalf("one DDL produced %d finding identities: %v", len(ids), ids)
	}
	reused := waitingDDL(200)
	reused.BackendStart = ddlStart.Add(time.Hour)
	ddls = []activeDDL{reused}
	_, _ = d.PollOnce(context.Background())
	if *analyses != 2 {
		t.Fatalf("reused pid with a new backend analysed %d times total, want 2",
			*analyses)
	}
}

// QueuedDDLPID tells the lock-chain probe which chain is a queue behind
// a tracked DDL; it follows the detector's last poll.
func TestDetector_QueuedDDLPID(t *testing.T) {
	ddls := []activeDDL{waitingDDL(200)}
	d, _, _ := queueDetector(&ddls, warningIncident, &fakeQueueSource{})
	if _, ok := d.QueuedDDLPID(queueChain(200, 3)); ok {
		t.Fatal("queued before any poll")
	}
	_, _ = d.PollOnce(context.Background())
	if pid, ok := d.QueuedDDLPID(queueChain(200, 3)); !ok || pid != 200 {
		t.Fatalf("QueuedDDLPID(3 waiters) = %d, %v; want 200, true", pid, ok)
	}
	if _, ok := d.QueuedDDLPID(queueChain(200, 2)); ok {
		t.Fatal("2 waiters reported as a queue (threshold 3)")
	}
	if _, ok := d.QueuedDDLPID(queueChain(201, 9)); ok {
		t.Fatal("untracked pid reported as a queued DDL")
	}
	ddls = nil
	_, _ = d.PollOnce(context.Background())
	if _, ok := d.QueuedDDLPID(queueChain(200, 3)); ok {
		t.Fatal("finished DDL still reported as queued")
	}
	var nilDetector *Detector
	if _, ok := nilDetector.QueuedDDLPID(queueChain(200, 3)); ok {
		t.Fatal("nil detector reported a queued DDL")
	}
}

// Invalid input: a configured threshold is honoured; zero or negative
// falls back to the default of 3.
func TestDetector_QueueWaitersThreshold(t *testing.T) {
	for _, tc := range []struct {
		cfg, waiters int
		want         bool
	}{
		{0, 3, true}, {-1, 2, false}, {5, 4, false}, {5, 5, true}, {1, 1, true},
	} {
		ddls := []activeDDL{waitingDDL(200)}
		d, _, _ := queueDetector(&ddls, warningIncident, &fakeQueueSource{})
		d.cfg.QueueWaitersThreshold = tc.cfg
		_, _ = d.PollOnce(context.Background())
		if _, ok := d.QueuedDDLPID(queueChain(200, tc.waiters)); ok != tc.want {
			t.Fatalf("threshold %d, %d waiters: queued=%v want %v", tc.cfg,
				tc.waiters, ok, tc.want)
		}
	}
}

// Concurrent access: the probe asks while the detector polls.
func TestDetector_QueuedDDLPIDConcurrent(t *testing.T) {
	ddls := []activeDDL{waitingDDL(200)}
	d, _, _ := queueDetector(&ddls, warningIncident, &fakeQueueSource{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = d.PollOnce(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			d.QueuedDDLPID(queueChain(200, 3))
		}
	}()
	wg.Wait()
	if _, ok := d.QueuedDDLPID(queueChain(200, 3)); !ok {
		t.Fatal("tracked DDL lost after concurrent polls")
	}
}

// Error propagation: a failed activity read is returned with context; a
// failed analysis is logged and the other DDLs are still evaluated.
func TestDetector_ActivityAndAnalysisErrors(t *testing.T) {
	ddls := []activeDDL{waitingDDL(200)}
	d, sink, _ := queueDetector(&ddls, warningIncident, nil)
	d.activity = func(context.Context) ([]activeDDL, error) {
		return nil, errors.New("connection refused")
	}
	_, err := d.PollOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ddl activity") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("PollOnce error = %v, want the activity read failure", err)
	}
	other := waitingDDL(201)
	ddls = []activeDDL{waitingDDL(200), other}
	d.activity = func(context.Context) ([]activeDDL, error) { return ddls, nil }
	d.analyze = func(_ context.Context, a activeDDL) (*rca.Incident, *DDLRisk, error) {
		if a.PID == 200 {
			return nil, nil, errors.New("assessment failed")
		}
		return warningIncident(), nil, nil
	}
	if _, err := d.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce with one failed analysis: %v", err)
	}
	if sink.count() != 1 || sink.last(t).Detail["pid"] != 201 {
		t.Fatalf("persisted %d findings, want only pid 201's", sink.count())
	}
}
