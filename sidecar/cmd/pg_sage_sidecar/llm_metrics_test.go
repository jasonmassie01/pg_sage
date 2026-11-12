package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// observability-05: LLM health per registered client, in every mode.
// Integration on PostgreSQL does not apply: no database is involved, and
// the fake HTTP endpoint is the real transport.

// llmMetricsFake answers like observerFake in internal/llm: "marker-rate"
// is a 429, "marker-server" a 500, "marker-slow" answers after 2 s,
// "marker-empty" is blank, anything else a completion with usage 7/3/10.
func llmMetricsFake(t *testing.T) *httptest.Server {
	t.Helper()
	ok := `{"choices":[{"message":{"content":"%s"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Retry-After", "0")
		switch {
		case strings.Contains(body, "marker-rate"):
			w.WriteHeader(http.StatusTooManyRequests)
		case strings.Contains(body, "marker-server"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.Contains(body, "marker-slow"):
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		case strings.Contains(body, "marker-empty"):
			fmt.Fprintf(w, ok, "", 2, 0, 2)
		default:
			fmt.Fprintf(w, ok, "fine", 7, 3, 10)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withLLMHealth gives the test its own client registry and aggregator,
// bound to an enabled parent config aimed at endpoint.
func withLLMHealth(t *testing.T, endpoint string, budget int) config.LLMConfig {
	t.Helper()
	oldRegistry, oldHealth := llmClients, llmHealth
	t.Cleanup(func() { llmClients, llmHealth = oldRegistry, oldHealth })
	llmClients, llmHealth = &llmClientRegistry{}, newLLMHealthMetrics()
	parent := config.LLMConfig{Enabled: true, Endpoint: endpoint, APIKey: "k",
		Model: "test-model", TimeoutSeconds: 1, TokenBudgetDaily: budget,
		CooldownSeconds: 60}
	parent.OptimizerLLM.Enabled = true
	llmClients.bind(parent)
	return parent
}

// sharedLLM is the labels of the shared general client's series.
const sharedLLM = `{client="general",database=""}`

// registerSharedLLMClient tracks llmClient in a registry of the test's
// own, as registerLLMConfigOwner does at startup.
func registerSharedLLMClient(t *testing.T) {
	t.Helper()
	withLLMHealth(t, "http://127.0.0.1:1", 0)
	llmClients.add(llmClient, llmRoleGeneral, "", true)
}

// llmSample is the value of family's sample whose labels include want.
func llmSample(t *testing.T, e exposition, family string, want map[string]string) float64 {
	t.Helper()
	var found []*dto.Metric
	for _, m := range e.families[family].GetMetric() {
		if labelsMatch(m, want) {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s%v: %d samples, want 1\n%s", family, want, len(found),
			grepLines(e.body, family))
	}
	m := found[0]
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	}
	return m.GetGauge().GetValue()
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	matched := 0
	for _, l := range m.GetLabel() {
		if v, ok := want[l.GetName()]; ok {
			if v != l.GetValue() {
				return false
			}
			matched++
		}
	}
	return matched == len(want)
}

func llmKey(client, database string, extra ...string) map[string]string {
	out := map[string]string{"client": client, "database": database}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i]] = extra[i+1]
	}
	return out
}

// Error propagation: a 429, a 500, a timeout, a blank answer and a success
// through a registry client each move their result by exactly 1.
func TestLLMMetricsCountEachResultOnce(t *testing.T) {
	installHealthFleet(t, "fleet")
	withLLMHealth(t, llmMetricsFake(t).URL, 1_000_000)
	client := llmClients.newClient(llmRoleGeneral, "orders", true)
	// ok between the failures keeps the breaker closed (3 in a row open it).
	for _, marker := range []string{"rate", "server", "ok", "slow", "empty"} {
		_, _, _ = client.Chat(context.Background(), "s", "marker-"+marker, 50)
	}
	e := lintScrape(t, scrapeMetrics(t))
	want := map[string]float64{"ok": 1, "error": 1, "timeout": 1, "rate_limited": 1,
		"empty": 1, "budget_refused": 0, "circuit_open": 0, "cancelled": 0}
	for result, n := range want {
		got := llmSample(t, e, "pg_sage_llm_requests_total",
			llmKey("general", "orders", "result", result))
		if got != n {
			t.Errorf("requests_total{result=%q} = %v, want %v", result, got, n)
		}
	}
	if got := llmSample(t, e, "pg_sage_llm_request_duration_seconds",
		llmKey("general", "orders")); got != 5 {
		t.Errorf("duration histogram count = %v, want 5 provider exchanges", got)
	}
	for kind, n := range map[string]float64{"prompt": 9, "completion": 3,
		"unattributed": 0} {
		if got := llmSample(t, e, "pg_sage_llm_tokens_total",
			llmKey("general", "orders", "kind", kind)); got != n {
			t.Errorf("tokens_total{kind=%q} = %v, want %v", kind, got, n)
		}
	}
	if got := llmSample(t, e, "pg_sage_llm_tokens_used_today",
		llmKey("general", "orders")); got != 12 {
		t.Errorf("tokens_used_today = %v, want 12 (10 + the blank answer's 2)", got)
	}
}

// Modes: fleet and meta scrapes carry every registered client, the shared
// one (database "") and each database's general and optimizer clients.
// Fails before observability-05: only the shared client, unlabelled.
func TestLLMMetricsEveryClientInFleetAndMeta(t *testing.T) {
	for _, mode := range []string{"fleet", config.ModeMeta} {
		t.Run(mode, func(t *testing.T) {
			installContractFleet(t, mode, "orders")
			parent := withLLMHealth(t, llmMetricsFake(t).URL, 1000)
			cfg.LLM.OptimizerLLM.Enabled = true
			llmClient = llm.New(&parent, logStructuredWrapper)
			llmClients.add(llmClient, llmRoleGeneral, "", true)
			newFleetDBLLMClients("orders", true)
			e := lintScrape(t, scrapeMetrics(t))
			for _, key := range []map[string]string{llmKey("general", "orders"),
				llmKey("optimizer", "orders"), llmKey("general", "")} {
				if got := llmSample(t, e, "pg_sage_llm_circuit_open", key); got != 0 {
					t.Errorf("circuit_open%v = %v, want 0", key, got)
				}
				if got := llmSample(t, e, "pg_sage_llm_enabled", key); got != 1 {
					t.Errorf("enabled%v = %v, want 1", key, got)
				}
			}
		})
	}
}

// Boundary: the budget gauge follows a live llm.token_budget_daily
// change, not the startup config. Fails before observability-05.
func TestLLMMetricsBudgetFollowsLiveReconfigure(t *testing.T) {
	installHealthFleet(t, "standalone")
	parent := withLLMHealth(t, llmMetricsFake(t).URL, 1000)
	cfg.LLM.TokenBudgetDaily = 1000
	llmClients.newClient(llmRoleGeneral, "", true)
	e := lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_tokens_budget_daily",
		llmKey("general", "")); got != 1000 {
		t.Fatalf("budget before = %v, want 1000", got)
	}
	parent.TokenBudgetDaily = 5000
	llmClients.reconfigure(parent)
	e = lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_tokens_budget_daily",
		llmKey("general", "")); got != 5000 {
		t.Errorf("budget after reconfigure = %v, want 5000 (startup config is %d)",
			got, cfg.LLM.TokenBudgetDaily)
	}
	parent.TokenBudgetDaily = 0
	llmClients.reconfigure(parent)
	e = lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_tokens_budget_daily",
		llmKey("general", "")); got != 0 {
		t.Errorf("unlimited budget = %v, want 0", got)
	}
}

// State transitions: a retired runtime's clients leave the registry, the
// database's counters do not go backwards, and the next runtime's calls
// add to them; a database that leaves the process loses its series.
func TestLLMMetricsRetiredRuntimeKeepsCounters(t *testing.T) {
	installHealthFleet(t, "fleet")
	withLLMHealth(t, llmMetricsFake(t).URL, 1_000_000)
	okKey := llmKey("general", "orders", "result", "ok")
	old := llmClients.newClient(llmRoleGeneral, "orders", true)
	for i := range 2 {
		_, _, _ = old.Chat(context.Background(), "s", fmt.Sprintf("marker-ok %d", i), 50)
	}
	llmClients.remove(old)
	e := lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_requests_total", okKey); got != 2 {
		t.Fatalf("requests after retiring = %v, want 2", got)
	}
	if n := len(e.labelValues("pg_sage_llm_enabled", "database")); n != 0 {
		t.Errorf("retired client still has gauges: %d samples", n)
	}
	next := llmClients.newClient(llmRoleGeneral, "orders", true)
	_, _, _ = next.Chat(context.Background(), "s", "marker-ok next", 50)
	e = lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_requests_total", okKey); got != 3 {
		t.Errorf("requests after the next runtime = %v, want 3", got)
	}
	llmClients.remove(next)
	forgetRemovedDatabase("orders")
	e = lintScrape(t, scrapeMetrics(t))
	if hasLabelValue(e, "orders") {
		t.Errorf("removed database keeps series:\n%s", grepLines(e.body, `"orders"`))
	}
}

// Boundary: an old and a new client of the same database registered at
// once (a runtime replacement in progress) render one series per label
// set, from the newest client.
func TestLLMMetricsDuplicateRegistrationRendersOnce(t *testing.T) {
	installHealthFleet(t, "fleet")
	parent := withLLMHealth(t, llmMetricsFake(t).URL, 1000)
	llmClients.newClient(llmRoleGeneral, "orders", true)
	parent.TokenBudgetDaily = 2000
	newer := llm.New(&parent, logStructuredWrapper)
	llmClients.add(newer, llmRoleGeneral, "orders", true)
	e := lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_tokens_budget_daily",
		llmKey("general", "orders")); got != 2000 {
		t.Errorf("budget = %v, want the newest client's 2000", got)
	}
}

// Concurrent access: 64 parallel calls, scraped while they run, count 64.
func TestLLMMetricsConcurrentChats(t *testing.T) {
	installHealthFleet(t, "fleet")
	withLLMHealth(t, llmMetricsFake(t).URL, 1_000_000)
	client := llmClients.newClient(llmRoleGeneral, "orders", true)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = client.Chat(context.Background(), "s", fmt.Sprintf("marker-ok %d", i), 50)
		}()
	}
	for _, body := range concurrentScrapes(t, 8) {
		lintScrape(t, body)
	}
	wg.Wait()
	e := lintScrape(t, scrapeMetrics(t))
	key := llmKey("general", "orders", "result", "ok")
	if got := llmSample(t, e, "pg_sage_llm_requests_total", key); got != 64 {
		t.Errorf("requests_total = %v, want 64", got)
	}
	if got := llmSample(t, e, "pg_sage_llm_request_duration_seconds",
		llmKey("general", "orders")); got != 64 {
		t.Errorf("duration count = %v, want 64", got)
	}
}

// Nil/empty: with no LLM client the families still have HELP and TYPE,
// carry no sample, and the scrape parses.
func TestLLMMetricsNoClientHelpAndTypeOnly(t *testing.T) {
	installHealthFleet(t, "standalone")
	withLLMHealth(t, "http://127.0.0.1:1", 0)
	e := lintScrape(t, scrapeMetrics(t))
	for family, kind := range map[string]string{
		"pg_sage_llm_requests_total": "counter", "pg_sage_llm_tokens_total": "counter",
		"pg_sage_llm_request_duration_seconds": "histogram",
		"pg_sage_llm_enabled":                  "gauge", "pg_sage_llm_circuit_open": "gauge",
		"pg_sage_llm_tokens_used_today": "gauge", "pg_sage_llm_tokens_budget_daily": "gauge",
	} {
		if e.kinds[family] != kind {
			t.Errorf("%s TYPE = %q, want %s", family, e.kinds[family], kind)
		}
		n := len(sampleLines(e.body, family)) + len(sampleLines(e.body, family+"_bucket"))
		if n != 0 {
			t.Errorf("%s has %d samples without a client", family, n)
		}
	}
}

// CHECK-12: one client's series are bounded: 8 results, 8 buckets plus
// _sum and _count, 3 token kinds and 4 gauges.
func TestLLMMetricsSeriesPerClientBounded(t *testing.T) {
	installHealthFleet(t, "standalone")
	withLLMHealth(t, llmMetricsFake(t).URL, 1000)
	client := llmClients.newClient(llmRoleGeneral, "", true)
	_, _, _ = client.Chat(context.Background(), "s", "marker-ok", 50)
	body := lintScrape(t, scrapeMetrics(t)).body
	counts := map[string]int{}
	for _, family := range []string{"pg_sage_llm_requests_total",
		"pg_sage_llm_request_duration_seconds_bucket", "pg_sage_llm_request_duration_seconds_sum",
		"pg_sage_llm_request_duration_seconds_count", "pg_sage_llm_tokens_total",
		"pg_sage_llm_enabled", "pg_sage_llm_circuit_open"} {
		counts[family] = len(sampleLines(body, family))
	}
	want := map[string]int{"pg_sage_llm_requests_total": 8,
		"pg_sage_llm_request_duration_seconds_bucket": 8,
		"pg_sage_llm_request_duration_seconds_sum":    1,
		"pg_sage_llm_request_duration_seconds_count":  1, "pg_sage_llm_tokens_total": 3,
		"pg_sage_llm_enabled": 1, "pg_sage_llm_circuit_open": 1}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Errorf("series per client = %v, want %v", counts, want)
	}
	if !strings.Contains(body, `le="+Inf"} 1`) {
		t.Errorf("histogram lacks its +Inf bucket:\n%s",
			grepLines(body, "pg_sage_llm_request_duration_seconds"))
	}
}

// State transitions: scraping an open breaker after its cooldown does
// not close it or admit a probe; the gauge reports it lets requests in.
func TestLLMMetricsScrapeDoesNotCloseBreaker(t *testing.T) {
	installHealthFleet(t, "standalone")
	parent := withLLMHealth(t, llmMetricsFake(t).URL, 1_000_000)
	parent.CooldownSeconds = 1
	llmClients.reconfigure(parent)
	client := llmClients.newClient(llmRoleGeneral, "", true)
	for i := range 3 {
		_, _, _ = client.Chat(context.Background(), "s", fmt.Sprintf("marker-server %d", i), 50)
	}
	e := lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_circuit_open", llmKey("general", "")); got != 1 {
		t.Fatalf("circuit_open after 3 failures = %v, want 1", got)
	}
	time.Sleep(1100 * time.Millisecond)
	for range 100 {
		scrapeMetrics(t)
	}
	if got := llm.StatusOf(client).CircuitState; got != "open" {
		t.Errorf("breaker after 100 scrapes = %q, want still open (no probe yet)", got)
	}
	e = lintScrape(t, scrapeMetrics(t))
	if got := llmSample(t, e, "pg_sage_llm_requests_total",
		llmKey("general", "", "result", "error")); got != 3 {
		t.Errorf("requests_total{error} = %v, want 3 (scrapes are not requests)", got)
	}
}

// Boundary: a duration on a bucket bound falls in that bucket (le is
// inclusive), one past 120 s only in +Inf; tokens without a provider split
// are unattributed, and local refusals are counted but not timed.
func TestLLMHealthRecordBucketsAndTokenKinds(t *testing.T) {
	m := newLLMHealthMetrics()
	observe := m.observer("orders", "general")
	observe(llm.Outcome{Result: llm.ResultOK, Duration: 250 * time.Millisecond,
		PromptTokens: 4, CompletionTokens: 1, Tokens: 9})
	observe(llm.Outcome{Result: llm.ResultTimeout, Duration: 121 * time.Second})
	observe(llm.Outcome{Result: llm.ResultOK, Duration: 3 * time.Second, Tokens: 6})
	observe(llm.Outcome{Result: llm.ResultBudgetRefused, Duration: time.Hour})
	rows := m.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if fmt.Sprint(r.buckets) != "[1 0 0 0 1 0 0]" || r.count != 3 {
		t.Errorf("buckets = %v count %d, want [1 0 0 0 1 0 0] and 3", r.buckets, r.count)
	}
	if want := 0.25 + 121 + 3; r.sum != want {
		t.Errorf("sum = %v, want %v (the refusal's hour is not timed)", r.sum, want)
	}
	// llm-04 keys the token counters by purpose; these calls have none.
	tk := r.tokens[llm.PurposeUnlabeled]
	if tk.prompt != 4 || tk.completion != 1 || tk.unattributed != 4+6 {
		t.Errorf("tokens = %d/%d/%d, want 4/1/10", tk.prompt, tk.completion, tk.unattributed)
	}
	if r.results[llm.ResultOK] != 2 || r.results[llm.ResultBudgetRefused] != 1 {
		t.Errorf("results = %v", r.results)
	}
	m.prune("orders")
	observe(llm.Outcome{Result: llm.ResultOK})
	if rows := m.snapshot(); len(rows) != 0 {
		t.Errorf("a pruned database's late call resurrected its series: %+v", rows)
	}
}
