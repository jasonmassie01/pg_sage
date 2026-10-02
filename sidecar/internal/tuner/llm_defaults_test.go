package tuner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// tuner.llm_enabled now defaults to on, so the runtime hands the tuner a
// client even when no LLM is configured. The tuner must then use its
// deterministic rules silently: no provider call, no per-candidate log
// line and no llm_suppression finding.

type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) log(component, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, component+": "+fmt.Sprintf(format, args...))
}

func (c *capturedLog) containing(fragment string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range c.lines {
		if strings.Contains(line, fragment) {
			out = append(out, line)
		}
	}
	return out
}

func cleanSuppressions(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM sage.findings
		WHERE object_identifier LIKE 'llm_suppression:%'`); err != nil {
		t.Fatalf("clean suppression findings: %v", err)
	}
}

func suppressionCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.findings WHERE object_identifier LIKE 'llm_suppression:%'`,
	).Scan(&n); err != nil {
		t.Fatalf("count suppression findings: %v", err)
	}
	return n
}

var twoSymptoms = []PlanSymptom{{Kind: SymptomDiskSort}, {Kind: SymptomHashSpill}}

func TestDefaultLLMWithoutEndpointUsesDeterministicTunerSilently(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	cleanSuppressions(t, pool)
	defaults := config.DefaultConfig()
	if !defaults.LLM.Enabled || !defaults.Tuner.LLMEnabled {
		t.Fatal("precondition: llm.enabled and tuner.llm_enabled default on")
	}
	logs := &capturedLog{}
	client := llm.New(&defaults.LLM, logs.log)
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 2, WorkMemMaxMB: 512},
		nil, logs.log, WithLLM(client, nil))
	for i := 0; i < 3; i++ {
		rx := tu.tryLLMPrescribe(context.Background(),
			candidate{QueryID: 7001, Query: "SELECT 1"}, twoSymptoms, "")
		if len(rx) != 0 {
			t.Fatalf("cycle %d: prescriptions %+v without an LLM", i, rx)
		}
	}
	if n := suppressionCount(t, pool); n != 0 {
		t.Errorf("suppression findings = %d, want 0 without an LLM", n)
	}
	if lines := logs.containing("LLM"); len(lines) != 0 {
		t.Errorf("unconfigured LLM logged per candidate: %q", lines)
	}
}

// fakeTunerLLM is an OpenAI-compatible endpoint returning one valid
// pg_hint_plan prescription with the given token usage.
func fakeTunerLLM(t *testing.T, usage int) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	content := `[{"hint_directive":"HashJoin(o c)","rationale":"spill",` +
		`"confidence":0.9}]`
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				t.Errorf("provider path = %q", r.URL.Path)
			}
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%s},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":%d}}`,
				jsonString(content), usage)
		}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func defaultsWithEndpoint(url string, budget int) *config.LLMConfig {
	llmCfg := config.DefaultConfig().LLM
	llmCfg.Endpoint, llmCfg.APIKey, llmCfg.Model = url, "test-key", "m"
	llmCfg.TokenBudgetDaily = budget
	return &llmCfg
}

func TestDefaultLLMWithEndpointCallsProviderForHints(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	cleanSuppressions(t, pool)
	url, calls := fakeTunerLLM(t, 50)
	client := llm.New(defaultsWithEndpoint(url, config.DefaultLLMTokenBudget),
		noopLog2)
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 2, WorkMemMaxMB: 512},
		nil, noopLog2, WithLLM(client, nil))
	rx := tu.tryLLMPrescribe(context.Background(),
		candidate{QueryID: 7002, Query: "SELECT 2"}, twoSymptoms, "")
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	if len(rx) != 1 || rx[0].HintDirective != "HashJoin(o c)" {
		t.Fatalf("prescriptions = %+v, want the LLM hint", rx)
	}
	if client.TokensUsedToday() != 50 {
		t.Errorf("tokens charged = %d, want 50", client.TokensUsedToday())
	}
}

// A budget refusal is not a verdict on the query: no suppression.
func TestTunerBudgetRefusalDoesNotSuppressQuery(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	cleanSuppressions(t, pool)
	url, calls := fakeTunerLLM(t, 50)
	client := llm.New(defaultsWithEndpoint(url, 5), noopLog2)
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 2, WorkMemMaxMB: 512},
		nil, noopLog2, WithLLM(client, nil))
	rx := tu.tryLLMPrescribe(context.Background(),
		candidate{QueryID: 7003, Query: "SELECT 3"}, twoSymptoms, "")
	if len(rx) != 0 {
		t.Fatalf("prescriptions = %+v over budget, want none", rx)
	}
	if calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0 (budget refuses first)", calls.Load())
	}
	if n := suppressionCount(t, pool); n != 0 {
		t.Errorf("suppression findings = %d after a budget refusal, want 0", n)
	}
}

// Once the daily budget is spent the tuner stops trying, quietly.
func TestTunerSkipsLLMAfterDailyBudgetSpent(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	cleanSuppressions(t, pool)
	url, calls := fakeTunerLLM(t, 6000)
	logs := &capturedLog{}
	client := llm.New(defaultsWithEndpoint(url, 5000), logs.log)
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 2, WorkMemMaxMB: 512},
		nil, logs.log, WithLLM(client, nil))
	ctx := context.Background()
	tu.tryLLMPrescribe(ctx, candidate{QueryID: 7004, Query: "SELECT 4"},
		twoSymptoms, "")
	if !client.IsBudgetExhausted() || calls.Load() != 1 {
		t.Fatalf("precondition: exhausted=%v calls=%d, want true/1",
			client.IsBudgetExhausted(), calls.Load())
	}
	before := suppressionCount(t, pool)
	rx := tu.tryLLMPrescribe(ctx, candidate{QueryID: 7005, Query: "SELECT 5"},
		twoSymptoms, "")
	if len(rx) != 0 || calls.Load() != 1 {
		t.Fatalf("rx=%+v calls=%d after budget spent, want none/1", rx, calls.Load())
	}
	if after := suppressionCount(t, pool); after != before {
		t.Errorf("suppression findings %d -> %d after budget spent", before, after)
	}
	if lines := logs.containing("LLM prescribe failed"); len(lines) != 0 {
		t.Errorf("exhausted budget logged per candidate: %q", lines)
	}
}

// A usable fallback still serves when the primary is unconfigured.
func TestTunerUsesConfiguredFallbackWhenPrimaryUnconfigured(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	cleanSuppressions(t, pool)
	url, calls := fakeTunerLLM(t, 50)
	primary := llm.New(&config.DefaultConfig().LLM, noopLog2)
	fallback := llm.New(defaultsWithEndpoint(url, 100000), noopLog2)
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 2, WorkMemMaxMB: 512},
		nil, noopLog2, WithLLM(primary, fallback))
	rx := tu.tryLLMPrescribe(context.Background(),
		candidate{QueryID: 7006, Query: "SELECT 6"}, twoSymptoms, "")
	if calls.Load() != 1 || len(rx) != 1 {
		t.Fatalf("calls=%d rx=%+v, want the fallback to answer", calls.Load(), rx)
	}
}

// Post-test audit: an open circuit breaker also skips LLM work quietly.
func TestTunerSkipsLLMWhileCircuitOpen(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	cleanSuppressions(t, pool)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
	defer srv.Close()
	client := llm.New(defaultsWithEndpoint(srv.URL, 100000), noopLog2)
	for i := 0; !client.IsCircuitOpen() && i < 5; i++ {
		_, _, _ = client.Chat(context.Background(), "s", fmt.Sprintf("u%d", i), 10)
	}
	if !client.IsCircuitOpen() {
		t.Fatalf("precondition: circuit still closed after %d calls", calls.Load())
	}
	before := calls.Load()
	logs := &capturedLog{}
	tu := New(pool, TunerConfig{CascadeCooldownCycles: 2, WorkMemMaxMB: 512},
		nil, logs.log, WithLLM(client, nil))
	rx := tu.tryLLMPrescribe(context.Background(),
		candidate{QueryID: 7007, Query: "SELECT 7"}, twoSymptoms, "")
	if len(rx) != 0 || calls.Load() != before {
		t.Fatalf("rx=%+v calls %d->%d with the circuit open", rx, before, calls.Load())
	}
	if n := suppressionCount(t, pool); n != 0 {
		t.Errorf("suppression findings = %d with the circuit open, want 0", n)
	}
	if lines := logs.containing("LLM prescribe failed"); len(lines) != 0 {
		t.Errorf("open circuit logged per candidate: %q", lines)
	}
}
