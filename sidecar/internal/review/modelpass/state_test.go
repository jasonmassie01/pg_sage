package modelpass

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// Error propagation through the real client, budgets, state transitions
// and concurrency (review-03a).

// providerServer answers every chat completion with status and a
// Retry-After of 0, counting requests.
func providerServer(t *testing.T, status int, delay time.Duration) (*httptest.Server,
	*atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func realClient(url string, timeout int) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: url, APIKey: "test-key",
		Model: "fake-model", TimeoutSeconds: timeout, CooldownSeconds: 0},
		func(string, string, ...any) {})
}

// The tool-calling transport never retries a 429 (llm.ChatWithTools: the
// caller falls back inside its own deadline), so a rate-limited run stops
// at once with the provider's class, and the pass falls back.
func TestReview_RateLimitStopsTheRunWithClassRate(t *testing.T) {
	srv, n := providerServer(t, http.StatusTooManyRequests, 0)
	in := input(t, "db1")
	store := &memStore{}
	rep, err := Review(context.Background(), settings(1, "majority"),
		deps(realClient(srv.URL, 5), store), in)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("the provider saw %d requests, want one: tool calls are not retried", n.Load())
	}
	runs := store.all()
	if len(runs) != 1 || runs[0].ErrorClass != "rate" || rep.ModelPass != report.PassFailed ||
		!strings.Contains(runs[0].Reason, agentloop.StopRateLimited) {
		t.Fatalf("runs = %+v, pass %s, want one failed run of class rate", runs, rep.ModelPass)
	}
	checkFallback(t, rep, in)
}

func TestReview_StepTimeoutIsRecordedAsTimeout(t *testing.T) {
	srv, _ := providerServer(t, http.StatusOK, 3*time.Second)
	in := input(t, "db1")
	store := &memStore{}
	d := deps(realClient(srv.URL, 30), store)
	d.Budget.StepTimeout = 200 * time.Millisecond
	d.Budget.MaxSteps = 1
	rep, err := Review(context.Background(), settings(1, "majority"), d, in)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	runs := store.all()
	if len(runs) != 1 || runs[0].ErrorClass != "timeout" || rep.ModelPass != report.PassFailed {
		t.Fatalf("runs = %+v, pass %s, want one failed run of class timeout", runs, rep.ModelPass)
	}
}

func TestReview_BudgetExhaustionSkipsThePass(t *testing.T) {
	in := input(t, "db1")
	store := &memStore{}
	rep, err := Review(context.Background(), settings(3, "majority"),
		deps(failing(llm.ErrBudgetExhausted), store), in)
	if err != nil || rep.ModelPass != report.PassSkipped ||
		rep.ModelPassReason != "token budget" {
		t.Fatalf("pass = %s %q (%v), want skipped: token budget", rep.ModelPass,
			rep.ModelPassReason, err)
	}
	for _, r := range store.all() {
		if r.Status != StatusSkipped || r.Reason != "token budget" {
			t.Errorf("run = %+v, want skipped for the token budget", r)
		}
	}
	checkFallback(t, rep, in)
}

func TestTracker_LegalAndIllegalMoves(t *testing.T) {
	tr := newTracker()
	if tr.Status() != report.PassPending {
		t.Fatalf("start = %s", tr.Status())
	}
	if err := tr.advance(report.PassDone); err == nil {
		t.Fatal("pending -> done was allowed")
	}
	for _, to := range []report.PassStatus{report.PassRunning, report.PassDone} {
		if err := tr.advance(to); err != nil {
			t.Fatalf("move to %s: %v", to, err)
		}
	}
	if err := tr.advance(report.PassRunning); err == nil {
		t.Fatal("done -> running was allowed")
	}
	if got := tr.History(); len(got) != 3 || got[0] != report.PassPending ||
		got[2] != report.PassDone {
		t.Fatalf("history = %v", got)
	}
	skip := newTracker()
	if err := skip.advance(report.PassSkipped); err != nil {
		t.Fatalf("pending -> skipped: %v", err)
	}
	fail := newTracker()
	if fail.advance(report.PassRunning) != nil || fail.advance(report.PassFailed) != nil ||
		fail.advance(report.PassSkipped) == nil {
		t.Fatal("failed is not final")
	}
}

func TestReview_ReportsItsStateHistory(t *testing.T) {
	in := input(t, "db1")
	var seen []report.PassStatus
	d := deps(always(keepRanked(t, in, "t_a")), nil)
	d.OnState = func(s report.PassStatus) { seen = append(seen, s) }
	if _, err := Review(context.Background(), settings(1, "majority"), d, in); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != report.PassPending || seen[1] != report.PassRunning ||
		seen[2] != report.PassDone {
		t.Fatalf("states = %v, want pending, running, done", seen)
	}
}

// Turning review.model_pass off mid-review finishes the run in flight and
// skips the rest.
func TestReview_TurnedOffMidReviewFinishesTheCurrentRun(t *testing.T) {
	in := input(t, "db1")
	var mode atomic.Value
	mode.Store("on")
	m := &scripted{reply: func(int, []llm.Message) (llm.ToolResult, error) {
		mode.Store("off")
		return native(keepRanked(t, in, "t_a")), nil
	}}
	store := &memStore{}
	d := deps(m, store)
	d.Parallel = 1
	d.Mode = func() string { return mode.Load().(string) }
	s := settings(3, "majority")
	s.Mode = "on"
	rep, err := Review(context.Background(), s, d, in)
	if err != nil || rep.ModelPass != report.PassSkipped || m.count() != 1 ||
		!strings.Contains(rep.ModelPassReason, "off mid-review") {
		t.Fatalf("pass = %s %q after %d calls (%v)", rep.ModelPass, rep.ModelPassReason,
			m.count(), err)
	}
	statuses := map[string]int{}
	for _, r := range store.all() {
		statuses[r.Status]++
	}
	if statuses[StatusDone] != 1 || statuses[StatusSkipped] != 2 {
		t.Fatalf("stored statuses = %v, want 1 done and 2 skipped", statuses)
	}
	checkFallback(t, rep, in)
}

// Three runs are in flight at once over one shared seed, which no run
// changes.
func TestReview_RunsExecuteInParallelOverASharedSeed(t *testing.T) {
	in := input(t, "db1")
	before, _, err := buildSeed(in)
	if err != nil {
		t.Fatal(err)
	}
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	m := &scripted{reply: func(int, []llm.Message) (llm.ToolResult, error) {
		if now := inFlight.Add(1); now > peak.Load() {
			peak.Store(now)
		}
		if inFlight.Load() == 3 {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		inFlight.Add(-1)
		return native(keepRanked(t, in, "t_a")), nil
	}}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || rep.ModelPass != report.PassDone || peak.Load() != 3 {
		t.Fatalf("pass %s, peak %d in flight (%v), want 3 runs at once", rep.ModelPass,
			peak.Load(), err)
	}
	after, _, _ := buildSeed(in)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("the runs changed the shared seed")
	}
}

// Two reviews of different databases at once never share a transcript.
func TestReview_TwoDatabasesNeverShareATranscript(t *testing.T) {
	in1, in2 := input(t, "orders"), input(t, "billing")
	m := &scripted{reply: func(_ int, msgs []llm.Message) (llm.ToolResult, error) {
		in := in1
		if strings.Contains(msgs[0].Content, `"billing"`) {
			in = in2
		}
		return native(keepRanked(t, in, "t_a")), nil
	}}
	var wg sync.WaitGroup
	reps := make([]report.Report, 2)
	for i, in := range []Input{in1, in2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reps[i], _ = Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
		}()
	}
	wg.Wait()
	if reps[0].Database != "orders" || reps[1].Database != "billing" ||
		reps[0].ModelPass != report.PassDone || reps[1].ModelPass != report.PassDone {
		t.Fatalf("reports = %+v / %+v", reps[0].Database, reps[1].Database)
	}
	for _, c := range m.calls {
		sys, task := c.msgs[0].Content, c.msgs[1].Content
		for _, pair := range [][2]string{{"orders", "billing"}, {"billing", "orders"}} {
			if strings.Contains(sys, `"`+pair[0]+`"`) && strings.Contains(task, pair[1]) {
				t.Fatalf("a %s conversation names %s", pair[0], pair[1])
			}
		}
	}
	if m.count() != 6 {
		t.Fatalf("calls = %d, want 3 per review", m.count())
	}
}

var _ agentloop.Model = (*llm.Client)(nil)
