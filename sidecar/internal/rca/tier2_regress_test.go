package rca

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

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// Regression tests for substrate-B2 (Tier 2 unreachable), G3-B17 (LLM call
// under Engine.mu, no caller context, no dedup before the call) and
// G3-B27 (positional chain attribution, "; "-joined recommended SQL).

// capturingLLMServer answers every request with content and records the
// request bodies so tests can inspect the prompt.
type capturingLLMServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	calls  atomic.Int32
}

func newCapturingLLMServer(content string) *capturingLLMServer {
	c := &capturingLLMServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			c.mu.Lock()
			c.bodies = append(c.bodies, string(b))
			c.mu.Unlock()
			c.calls.Add(1)
			writeChatResponse(w, content)
		}))
	return c
}

func (c *capturingLLMServer) allBodies() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.bodies, "\n")
}

func writeChatResponse(w http.ResponseWriter, content string) {
	resp := map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]string{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"total_tokens": 50},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func tier2JSON(t *testing.T, resp any) string {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tier2 response: %v", err)
	}
	return string(b)
}

// realTier2Inputs builds snapshots whose real detectors fire
// idle_in_tx_elevated (no Tier 1 tree consumes it when connections_high
// is quiet), wal_growth_spike and lock_contention: three co-occurring
// production signals, exactly one of them unexplained by Tier 1.
func realTier2Inputs() (
	curr, prev *collector.Snapshot, lcf []analyzer.Finding,
) {
	now := time.Now()
	sys := collector.SystemStats{
		TotalBackends: 10, MaxConnections: 100, CacheHitRatio: 0.999,
	}
	prev = &collector.Snapshot{
		CollectedAt: now.Add(-time.Minute),
		System:      sys,
		Queries:     []collector.QueryStats{{QueryID: 1, WALBytes: 1000}},
	}
	curr = &collector.Snapshot{
		CollectedAt: now,
		System:      sys,
		Queries:     []collector.QueryStats{{QueryID: 1, WALBytes: 5000}},
		ConfigData: &collector.ConfigSnapshot{
			ConnectionStates: []collector.ConnectionState{{
				State: "idle in transaction", Count: 4,
				AvgDurationSeconds: 900,
			}},
		},
	}
	lcf = []analyzer.Finding{{
		Category: "lock_chain", Severity: "warning",
		Detail: map[string]any{"total_blocked": 3},
	}}
	return curr, prev, lcf
}

func llmIncidents(incs []Incident) []Incident {
	var out []Incident
	for _, inc := range incs {
		if inc.Source == "llm" {
			out = append(out, inc)
		}
	}
	return out
}

func TestTier2_FiresOnRealCoOccurringSignals(t *testing.T) {
	srv := newCapturingLLMServer(tier2JSON(t, validTier2Response()))
	defer srv.srv.Close()

	eng := testEngine()
	eng.cfg.LLMCorrelationThreshold = 3
	eng.WithLLM(tier2LLMClient(srv.srv.URL))

	curr, prev, lcf := realTier2Inputs()
	incs := eng.Analyze(curr, prev, testConfig(), lcf)

	got := llmIncidents(incs)
	if len(got) != 1 {
		t.Fatalf("llm incidents = %d, want 1 (3 real co-occurring "+
			"signals, 1 unexplained); LLM calls = %d",
			len(got), srv.calls.Load())
	}
	if !stringsEqual(got[0].SignalIDs, []string{"idle_in_tx_elevated"}) {
		t.Errorf("SignalIDs = %v, want [idle_in_tx_elevated]",
			got[0].SignalIDs)
	}
	body := srv.allBodies()
	for _, want := range []string{
		"idle_in_tx_elevated", "wal_growth_spike", "lock_contention",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt missing co-occurring signal %q", want)
		}
	}
}

func TestTier2_NoCallWhenEverySignalExplained(t *testing.T) {
	srv := newCapturingLLMServer(tier2JSON(t, validTier2Response()))
	defer srv.srv.Close()

	eng := testEngine()
	eng.cfg.LLMCorrelationThreshold = 2
	eng.WithLLM(tier2LLMClient(srv.srv.URL))

	curr, prev, lcf := realTier2Inputs()
	curr.ConfigData = nil // idle_in_tx_elevated no longer fires
	incs := eng.Analyze(curr, prev, testConfig(), lcf)

	if n := srv.calls.Load(); n != 0 {
		t.Fatalf("LLM calls = %d, want 0 when Tier 1 explains all", n)
	}
	if got := llmIncidents(incs); len(got) != 0 {
		t.Fatalf("llm incidents = %d, want 0", len(got))
	}
}

func TestTier2_LLMCallDoesNotHoldEngineMutex(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	content := tier2JSON(t, validTier2Response())
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case received <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeChatResponse(w, content)
		}))
	defer srv.Close()
	defer doRelease()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.URL))
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})

	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
	}()

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("Tier 2 LLM request never arrived")
	}

	got := make(chan int, 1)
	go func() { got <- len(eng.ActiveIncidents()) }()
	select {
	case <-got:
	case <-time.After(300 * time.Millisecond):
		doRelease()
		<-done
		t.Fatal("ActiveIncidents blocked while the Tier 2 LLM call " +
			"was in flight: LLM called under Engine.mu")
	}
	doRelease()
	<-done
}

func TestTier2_CanceledCallerContextSkipsLLM(t *testing.T) {
	srv := newCapturingLLMServer(tier2JSON(t, validTier2Response()))
	defer srv.srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.srv.URL))
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	incs := eng.AnalyzeContext(
		ctx, quietSnapshot(), quietSnapshot(), testConfig(), nil)

	if n := srv.calls.Load(); n != 0 {
		t.Errorf("LLM calls = %d, want 0 with canceled caller ctx", n)
	}
	if got := llmIncidents(incs); len(got) != 0 {
		t.Errorf("llm incidents = %d, want 0", len(got))
	}
}

func TestTier2_DedupBeforeCallingLLMAgain(t *testing.T) {
	srv := newCapturingLLMServer(tier2JSON(t, validTier2Response()))
	defer srv.srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.srv.URL))
	src := &mockLogSource{}
	eng.SetLogSource(src)

	var last []Incident
	for i := 0; i < 2; i++ {
		src.signals = uncoveredSignals(3)
		last = eng.Analyze(
			quietSnapshot(), quietSnapshot(), testConfig(), nil)
	}

	if n := srv.calls.Load(); n != 1 {
		t.Errorf("LLM calls = %d, want 1 (open llm incident already "+
			"covers the same signal set)", n)
	}
	got := llmIncidents(last)
	if len(got) != 1 {
		t.Fatalf("llm incidents = %d, want 1", len(got))
	}
	if got[0].OccurrenceCount != 2 {
		t.Errorf("OccurrenceCount = %d, want 2", got[0].OccurrenceCount)
	}
}

func TestTier2_ChainStepsNotAttributedByIndex(t *testing.T) {
	resp := validTier2Response()
	resp.CausalChain = "A -> B -> C"
	srv := newCapturingLLMServer(tier2JSON(t, resp))
	defer srv.srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.srv.URL))
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	got := llmIncidents(eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil))
	if len(got) != 1 {
		t.Fatalf("llm incidents = %d, want 1", len(got))
	}
	if len(got[0].CausalChain) != 3 {
		t.Fatalf("chain len = %d, want 3", len(got[0].CausalChain))
	}
	for i, link := range got[0].CausalChain {
		if link.Signal != "" {
			t.Errorf("chain[%d].Signal = %q, want empty: step text %q "+
				"names no signal", i, link.Signal, link.Description)
		}
	}
}

func TestTier2_StructuredStepsKeepOnlyRealSignalIDs(t *testing.T) {
	raw := `{"root_cause":"pool leak","severity":"warning",` +
		`"causal_steps":[` +
		`{"signal":"custom_signal_beta","description":"beta rose"},` +
		`{"signal":"invented_signal","description":"made up"}],` +
		`"recommended_sql":[],"action_risk":"low"}`
	srv := newCapturingLLMServer(raw)
	defer srv.srv.Close()

	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.srv.URL))
	eng.SetLogSource(&mockLogSource{signals: uncoveredSignals(3)})
	got := llmIncidents(eng.Analyze(
		quietSnapshot(), quietSnapshot(), testConfig(), nil))
	if len(got) != 1 {
		t.Fatalf("llm incidents = %d, want 1", len(got))
	}
	chain := got[0].CausalChain
	if len(chain) != 2 {
		t.Fatalf("chain len = %d, want 2: %+v", len(chain), chain)
	}
	if chain[0].Signal != "custom_signal_beta" {
		t.Errorf("chain[0].Signal = %q, want custom_signal_beta",
			chain[0].Signal)
	}
	if chain[1].Signal != "" {
		t.Errorf("chain[1].Signal = %q, want empty (not an input "+
			"signal)", chain[1].Signal)
	}
}

func TestTier2_RecommendedSQLNeverJoined(t *testing.T) {
	tests := []struct {
		name string
		sql  []string
		want string
	}{
		{"two statements keep first", []string{"SELECT 1", "VACUUM"},
			"SELECT 1"},
		{"trailing semicolon trimmed", []string{"SELECT 1;"}, "SELECT 1"},
		{"embedded multi-statement dropped",
			[]string{"SELECT 1; DROP TABLE t"}, ""},
		{"blank entries skipped", []string{"  ", "ANALYZE t"},
			"ANALYZE t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc := buildTier2Incident(tier2Response{
				RootCause: "x", RecommendedSQL: tt.sql,
			}, testSignals(1))
			if inc.RecommendedSQL != tt.want {
				t.Errorf("RecommendedSQL = %q, want %q",
					inc.RecommendedSQL, tt.want)
			}
		})
	}
}

func TestTier2_PromptRedactsLogFreeText(t *testing.T) {
	srv := newCapturingLLMServer(tier2JSON(t, validTier2Response()))
	defer srv.srv.Close()

	sigs := uncoveredSignals(3)
	sigs[0].Metrics = map[string]any{
		"message": "invalid input 'hunter2-secret' for column",
		"detail":  "Key (email)=(bob@example.com) already exists.",
		"user":    "alice_login",
		"query":   "SELECT * FROM p WHERE ssn = '123-45-6789'",
	}
	eng := testEngine()
	eng.WithLLM(tier2LLMClient(srv.srv.URL))
	eng.SetLogSource(&mockLogSource{signals: sigs})
	eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)

	if srv.calls.Load() != 1 {
		t.Fatalf("LLM calls = %d, want 1", srv.calls.Load())
	}
	body := srv.allBodies()
	for _, secret := range []string{
		"hunter2-secret", "bob@example.com", "alice_login", "123-45-6789",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("Tier 2 prompt leaked %q", secret)
		}
	}
}
