package migration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// migration-10: model work runs on one bounded worker per database, off
// the poll path, and one model call serves every detection of a statement
// until its table's schema changes.

// slowLLM answers after delay, or when the request is cancelled.
type slowLLM struct {
	delay   time.Duration
	reply   string
	started atomic.Int64
	srv     *httptest.Server
}

func newSlowLLM(t *testing.T, delay time.Duration, reply string) *slowLLM {
	t.Helper()
	s := &slowLLM{delay: delay, reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *slowLLM) serve(w http.ResponseWriter, req *http.Request) {
	_, _ = io.ReadAll(req.Body)
	s.started.Add(1)
	select {
	case <-time.After(s.delay):
	case <-req.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"content": s.reply},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"total_tokens": 10},
	})
}

func (s *slowLLM) client() *llm.Client {
	return llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: s.srv.URL, APIKey: "test-key", Model: "test-model",
		TimeoutSeconds: 60, TokenBudgetDaily: 1_000_000,
	}, nopMigrationLog)
}

// memorySink is a FindingSink that also merges model output the way the
// store does: a merge replaces the llm_ keys of the identified finding.
type memorySink struct {
	mu       sync.Mutex
	upserts  []MigrationSafetyFinding
	findings map[string]MigrationSafetyFinding
	merges   int
}

func newMemorySink() *memorySink {
	return &memorySink{findings: map[string]MigrationSafetyFinding{}}
}

func (s *memorySink) UpsertMigrationSafetyFinding(
	_ context.Context, f MigrationSafetyFinding,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, f)
	if old, ok := s.findings[f.ObjectIdentifier]; ok {
		for k, v := range old.Detail {
			if _, set := f.Detail[k]; !set && strings.HasPrefix(k, "llm_") {
				f.Detail[k] = v
			}
		}
	}
	s.findings[f.ObjectIdentifier] = f
	return int64(len(s.upserts)), nil
}

func (s *memorySink) MergeMigrationFindingLLMDetail(
	_ context.Context, identity string, fields map[string]any,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merges++
	f, ok := s.findings[identity]
	if !ok {
		return nil
	}
	for k := range f.Detail {
		if strings.HasPrefix(k, "llm_") {
			delete(f.Detail, k)
		}
	}
	for k, v := range fields {
		f.Detail[k] = v
	}
	return nil
}

func (s *memorySink) snapshot() (map[string]MigrationSafetyFinding, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]MigrationSafetyFinding, len(s.findings))
	for k, v := range s.findings {
		out[k] = v
	}
	return out, len(s.upserts), s.merges
}

func enrichCfg() *config.MigrationConfig {
	return &config.MigrationConfig{Enabled: true, Mode: "advisory",
		ActivityPolling: true, LogDetection: true}
}

func fixedActivity(ddls *[]activeDDL) func(context.Context) ([]activeDDL, error) {
	return func(context.Context) ([]activeDDL, error) { return *ddls, nil }
}

func llmKeys(detail map[string]any) []string {
	var keys []string
	for k := range detail {
		if strings.HasPrefix(k, "llm_") {
			keys = append(keys, k)
		}
	}
	return keys
}

// CHECK-29: with a model that takes 5 s, two polls that each see a new
// DDL return well under a second, and their deterministic findings are
// already stored.
func TestPollOnceDoesNotWaitForTheModel(t *testing.T) {
	slow := newSlowLLM(t, 5*time.Second, "```sql\nANALYZE orders;\n```")
	cfg := enrichCfg()
	adv := NewAdvisor(nil, cfg, 160004, "latency_db", nopMigrationLog, slow.client())
	sink := newMemorySink()
	var ddls []activeDDL
	d := NewDetector(nil, adv, cfg, nopMigrationLog).WithFindingSink(sink)
	d.activity = fixedActivity(&ddls)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go adv.RunEnrichment(ctx)

	for i, q := range []string{"VACUUM FULL orders", "VACUUM FULL customers"} {
		ddls = []activeDDL{{PID: 100 + i, Query: q}}
		start := time.Now()
		incs, err := d.PollOnce(ctx)
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("poll %d took %s with a 5 s model; want < 1 s", i+1, elapsed)
		}
		if err != nil || len(incs) != 1 {
			t.Fatalf("poll %d = %d incidents, %v; want 1 deterministic incident",
				i+1, len(incs), err)
		}
		if incs[0].RecommendedSQLSource != SQLSourceDeterministic {
			t.Errorf("poll %d source = %q, want deterministic", i+1,
				incs[0].RecommendedSQLSource)
		}
	}
	findings, upserts, _ := sink.snapshot()
	if upserts != 2 || len(findings) != 2 {
		t.Fatalf("upserts = %d findings = %d, want both deterministic findings stored",
			upserts, len(findings))
	}
	for id, f := range findings {
		if keys := llmKeys(f.Detail); len(keys) != 0 {
			t.Errorf("finding %s has model output %v before the model answered", id, keys)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for slow.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if slow.started.Load() == 0 {
		t.Fatal("the queued enrichment never asked the model")
	}
}

// Dedupe: the same statement seen by the activity and the log detector
// is one model call; a changed table schema is a second.
func TestEnrichmentCallsTheModelOncePerIdentity(t *testing.T) {
	rec := newRecordingLLM(t, "```sql\nANALYZE orders;\n```")
	cfg := enrichCfg()
	adv := NewAdvisor(nil, cfg, 160004, "dedupe_db", nopMigrationLog, rec.client())
	var schemaMu sync.Mutex
	schema := "-- Table: public.orders\n-- Columns:\n--   id integer\n"
	adv.scriptGen.schemaText = func(context.Context, string, string) string {
		schemaMu.Lock()
		defer schemaMu.Unlock()
		return schema
	}
	sink := newMemorySink()
	ddls := []activeDDL{{PID: 7, Query: "VACUUM FULL orders"}}
	d := NewDetector(nil, adv, cfg, nopMigrationLog).WithFindingSink(sink)
	d.activity = fixedActivity(&ddls)
	ld := NewLogDetector(adv, nopMigrationLog).WithFindingSink(sink)
	ctx := context.Background()

	if _, err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if inc := ld.processLogEntry(ctx, logwatch.LogEntry{PID: 7,
		Query: "VACUUM FULL orders"}); inc == nil {
		t.Fatal("log detector produced no deterministic incident")
	}
	if got := len(rec.calls()); got != 0 {
		t.Fatalf("model called %d times on the detection path, want 0", got)
	}
	adv.enricher.drain(ctx)
	if got := len(rec.calls()); got != 1 {
		t.Fatalf("model calls = %d after both detectors, want 1", got)
	}
	assertEnrichedOnce(t, sink)

	schemaMu.Lock()
	schema += "--   note text\n"
	schemaMu.Unlock()
	for _, want := range []int{2, 2} { // changed schema: a call; unchanged: cached
		ld.processLogEntry(ctx, logwatch.LogEntry{PID: 7, Query: "VACUUM FULL orders"})
		adv.enricher.drain(ctx)
		if got := len(rec.calls()); got != want {
			t.Fatalf("model calls = %d, want %d", got, want)
		}
	}
}

// assertEnrichedOnce: both detections stored one finding, and both
// enrichments merged model output into it.
func assertEnrichedOnce(t *testing.T, sink *memorySink) {
	t.Helper()
	findings, _, merges := sink.snapshot()
	if len(findings) != 1 || merges != 2 {
		t.Fatalf("findings = %d merges = %d, want one finding enriched twice from one call",
			len(findings), merges)
	}
	for _, f := range findings {
		if len(llmKeys(f.Detail)) == 0 {
			t.Errorf("finding detail has no model output: %v", f.Detail)
		}
	}
}

// Boundary: 16 jobs fit; the 17th evicts the oldest.
func TestEnrichQueueBoundary(t *testing.T) {
	var ran []int
	e := newEnricher("boundary_db", enrichQueueSize,
		func(_ context.Context, j enrichJob) { ran = append(ran, j.pid) }, nopMigrationLog)
	for i := 1; i <= enrichQueueSize; i++ {
		e.enqueue(enrichJob{pid: i})
	}
	if e.Dropped() != 0 {
		t.Fatalf("dropped = %d with exactly %d jobs, want 0", e.Dropped(), enrichQueueSize)
	}
	e.enqueue(enrichJob{pid: enrichQueueSize + 1})
	if e.Dropped() != 1 {
		t.Fatalf("dropped = %d after one overflow, want 1", e.Dropped())
	}
	e.drain(context.Background())
	if len(ran) != enrichQueueSize || ran[0] != 2 || ran[len(ran)-1] != enrichQueueSize+1 {
		t.Fatalf("ran = %v, want jobs 2..%d (the oldest dropped)", ran, enrichQueueSize+1)
	}
}

// 40 jobs against a queue of 16: 24 dropped, the newest 16 kept in order.
func TestEnrichQueueDropsTheOldestWhenFull(t *testing.T) {
	var ran []int
	e := newEnricher("drop_seq_db", enrichQueueSize,
		func(_ context.Context, j enrichJob) { ran = append(ran, j.pid) }, nopMigrationLog)
	for i := 1; i <= 40; i++ {
		e.enqueue(enrichJob{pid: i})
	}
	if e.Dropped() != 24 {
		t.Fatalf("dropped = %d, want 24", e.Dropped())
	}
	e.drain(context.Background())
	if len(ran) != 16 || ran[0] != 25 || ran[15] != 40 {
		t.Fatalf("ran = %v, want jobs 25..40", ran)
	}
	if got := droppedFor("drop_seq_db"); got != 24 {
		t.Fatalf("pg_sage_migration_enrich_dropped_total{database=drop_seq_db} = %d, want 24",
			got)
	}
}

// Concurrency: 40 parallel enqueues report 24 drops and keep 16 jobs.
func TestEnrichQueueConcurrentEnqueue(t *testing.T) {
	var ran atomic.Int64
	e := newEnricher("drop_conc_db", enrichQueueSize,
		func(context.Context, enrichJob) { ran.Add(1) }, nopMigrationLog)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 1; i <= 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			e.enqueue(enrichJob{pid: i})
		}(i)
	}
	close(start)
	wg.Wait()
	if e.Dropped() != 24 {
		t.Fatalf("dropped = %d, want 24", e.Dropped())
	}
	e.drain(context.Background())
	if ran.Load() != 16 {
		t.Fatalf("ran %d jobs, want 16", ran.Load())
	}
	if got := droppedFor("drop_conc_db"); got != 24 {
		t.Fatalf("dropped metric = %d, want 24", got)
	}
}

func droppedFor(db string) int64 {
	for _, c := range EnrichDroppedCounts() {
		if c.Database == db {
			return c.Count
		}
	}
	return -1
}

// The worker stops with its context and runs what was queued before.
func TestRunEnrichmentWorksTheQueueUntilCancelled(t *testing.T) {
	done := make(chan int, 4)
	e := newEnricher("run_db", enrichQueueSize,
		func(_ context.Context, j enrichJob) { done <- j.pid }, nopMigrationLog)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { e.Run(ctx); close(stopped) }()
	e.enqueue(enrichJob{pid: 1})
	e.enqueue(enrichJob{pid: 2})
	for want := 1; want <= 2; want++ {
		select {
		case got := <-done:
			if got != want {
				t.Fatalf("job %d ran, want %d (queue order)", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("job %d never ran", want)
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// Nil/zero: no LLM means no enricher; every entry point is a no-op.
func TestNoModelMeansNoEnrichment(t *testing.T) {
	adv := NewAdvisor(nil, enrichCfg(), 160004, "nollm_db", nopMigrationLog, nil)
	if adv.enricher != nil {
		t.Fatal("an advisor without a model has an enricher")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	returned := make(chan struct{})
	go func() { adv.RunEnrichment(ctx); close(returned) }()
	select {
	case <-returned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunEnrichment blocked without a model")
	}
	adv.queueEnrichment(1, "VACUUM FULL orders", nil, nil, newMemorySink())
	var e *Enricher
	e.enqueue(enrichJob{pid: 1})
	e.drain(ctx)
	if e.Dropped() != 0 {
		t.Fatal("nil enricher reports drops")
	}
	var nilAdv *Advisor
	nilAdv.RunEnrichment(ctx)
	nilAdv.queueEnrichment(1, "VACUUM FULL orders", nil, nil, nil)
}
