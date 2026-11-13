package main

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-05: LLM health per client, in every mode. Each registered
// client reports its calls here (llm.Client.SetObserver); the counters are
// keyed by (database, client), not by client, so they stay monotonic when
// a runtime replaces its clients, and leave /metrics only when the
// database leaves the process (forgetRemovedDatabase).

// llmLatencyBounds are the finite upper bounds of the request duration
// histogram, in seconds; with +Inf it has 8 buckets.
var llmLatencyBounds = []float64{0.25, 0.5, 1, 2.5, 10, 30, 120}

// llmLatencyResults are the results that time a provider exchange (an
// error can also be a pre-flight refusal of about 0 s); the budget and
// breaker refusals and cancellations would only add zeros.
var llmLatencyResults = map[llm.Result]bool{llm.ResultOK: true,
	llm.ResultError: true, llm.ResultTimeout: true, llm.ResultRateLimited: true,
	llm.ResultEmpty: true}

type llmHealthKey struct{ database, client string }

// llmHealthSeries is one key's counters; guarded by llmHealthMetrics.mu.
type llmHealthSeries struct {
	results map[llm.Result]uint64
	// buckets[i] counts durations in (bound i-1, bound i]; longer ones
	// are only in count.
	buckets []uint64
	sum     float64
	count   uint64
	// tokens is keyed by the calls' purpose (llm-04).
	tokens map[string]llmTokenKinds
}

// llmTokenKinds are one purpose's token counters.
type llmTokenKinds struct{ prompt, completion, unattributed uint64 }

type llmHealthMetrics struct {
	mu     sync.Mutex
	series map[llmHealthKey]*llmHealthSeries
}

var llmHealth = newLLMHealthMetrics()

func newLLMHealthMetrics() *llmHealthMetrics {
	return &llmHealthMetrics{series: map[llmHealthKey]*llmHealthSeries{}}
}

// observer is the function a client of database and role reports to. The
// key's series exists from now on, so a registered client shows zeros.
func (m *llmHealthMetrics) observer(database, client string) func(llm.Outcome) {
	key := llmHealthKey{database, client}
	m.mu.Lock()
	s := m.series[key]
	if s == nil {
		s = &llmHealthSeries{results: map[llm.Result]uint64{},
			buckets: make([]uint64, len(llmLatencyBounds)),
			tokens:  map[string]llmTokenKinds{}}
		m.series[key] = s
	}
	m.mu.Unlock()
	return func(o llm.Outcome) { m.record(s, o) }
}

func (m *llmHealthMetrics) record(s *llmHealthSeries, o llm.Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.results[o.Result]++
	recordTokens(s, o)
	if !llmLatencyResults[o.Result] {
		return
	}
	seconds := o.Duration.Seconds()
	s.sum += seconds
	s.count++
	if i, _ := slices.BinarySearch(llmLatencyBounds, seconds); i < len(s.buckets) {
		s.buckets[i]++
	}
}

// prune drops a database's series once it has left the process; a client
// of it still running reports to a series no scrape renders.
func (m *llmHealthMetrics) prune(database string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.series {
		if key.database == database {
			delete(m.series, key)
		}
	}
}

type llmHealthRow struct {
	key llmHealthKey
	llmHealthSeries
}

// snapshot copies every series, sorted by database, then client.
func (m *llmHealthMetrics) snapshot() []llmHealthRow {
	m.mu.Lock()
	rows := make([]llmHealthRow, 0, len(m.series))
	for key, s := range m.series {
		row := llmHealthRow{key: key, llmHealthSeries: *s}
		row.results = make(map[llm.Result]uint64, len(s.results))
		for r, n := range s.results {
			row.results[r] = n
		}
		row.buckets = slices.Clone(s.buckets)
		row.tokens = maps.Clone(s.tokens)
		rows = append(rows, row)
	}
	m.mu.Unlock()
	slices.SortFunc(rows, func(a, b llmHealthRow) int { return compareLLMKeys(a.key, b.key) })
	return rows
}

func compareLLMKeys(a, b llmHealthKey) int {
	return cmp.Or(cmp.Compare(a.database, b.database), cmp.Compare(a.client, b.client))
}

func (k llmHealthKey) labels() []labelPair {
	return []labelPair{{"client", k.client}, {"database", k.database}}
}

// llmClientGauge is one registered client's state at scrape time.
type llmClientGauge struct {
	key         llmHealthKey
	enabled     bool
	circuitOpen bool
	used        int64
	budget      int
}

// llmClientGauges reads every registered client. Two clients of one key
// (a runtime replacement in progress) render once, from the newest. The
// reads never change a breaker (IsCircuitOpen is a pure read, llm-02).
func llmClientGauges(entries []llmRegistryEntry) []llmClientGauge {
	newest := map[llmHealthKey]*llm.Client{}
	for _, e := range entries {
		newest[llmHealthKey{e.database, e.role.String()}] = e.client
	}
	out := make([]llmClientGauge, 0, len(newest))
	for key, c := range newest {
		out = append(out, llmClientGauge{key: key, enabled: c.IsEnabled(),
			circuitOpen: c.IsCircuitOpen(), used: c.TokensUsedToday(),
			budget: c.TokenBudgetDaily()})
	}
	slices.SortFunc(out, func(a, b llmClientGauge) int { return compareLLMKeys(a.key, b.key) })
	return out
}

// writeLLMHealthFromRegistry renders the LLM families from the process
// registry and aggregator.
func writeLLMHealthFromRegistry(b *strings.Builder) {
	entries, _ := llmClients.usageEntries()
	writeLLMHealthMetrics(b, llmHealth.snapshot(), llmClientGauges(entries))
}

func writeLLMHealthMetrics(b *strings.Builder, rows []llmHealthRow, gauges []llmClientGauge) {
	metricsreg.Header(b, "pg_sage_llm_requests_total")
	for _, row := range rows {
		for _, result := range llm.Results() {
			writeSample(b, "pg_sage_llm_requests_total",
				withLabel(row.key.labels(), "result", string(result)), row.results[result])
		}
	}
	b.WriteString("\n")
	writeLLMLatency(b, rows)
	writeLLMTokens(b, rows)
	writeLLMClientGauges(b, gauges)
}

// writeLLMLatency renders the duration histogram of the timed results.
func writeLLMLatency(b *strings.Builder, rows []llmHealthRow) {
	const name = "pg_sage_llm_request_duration_seconds"
	metricsreg.Header(b, name)
	for _, row := range rows {
		labels := row.key.labels()
		var cumulative uint64
		for i, bound := range llmLatencyBounds {
			cumulative += row.buckets[i]
			writeSample(b, name+"_bucket",
				withLabel(labels, "le", strconv.FormatFloat(bound, 'g', -1, 64)), cumulative)
		}
		writeSample(b, name+"_bucket", withLabel(labels, "le", "+Inf"), row.count)
		writeSample(b, name+"_sum", labels, row.sum)
		writeSample(b, name+"_count", labels, row.count)
	}
	b.WriteString("\n")
}

func writeLLMClientGauges(b *strings.Builder, gauges []llmClientGauge) {
	families := []struct {
		name  string
		value func(llmClientGauge) any
	}{
		{"pg_sage_llm_enabled", func(g llmClientGauge) any { return boolGauge(g.enabled) }},
		{"pg_sage_llm_circuit_open",
			func(g llmClientGauge) any { return boolGauge(g.circuitOpen) }},
		{"pg_sage_llm_tokens_used_today", func(g llmClientGauge) any { return g.used }},
		{"pg_sage_llm_tokens_budget_daily", func(g llmClientGauge) any { return g.budget }},
	}
	for _, f := range families {
		metricsreg.Header(b, f.name)
		for _, g := range gauges {
			writeSample(b, f.name, g.key.labels(), f.value(g))
		}
		b.WriteString("\n")
	}
}

func boolGauge(v bool) int {
	if v {
		return 1
	}
	return 0
}

// recordTokens adds a call's tokens under its purpose; tokens charged
// beyond the provider's split are unattributed (estimated or not split).
// A purpose that never spent a token has no series.
func recordTokens(s *llmHealthSeries, o llm.Outcome) {
	purpose := o.Purpose
	if !llm.ValidPurpose(purpose) {
		purpose = llm.PurposeUnlabeled
	}
	_, seen := s.tokens[purpose]
	if !seen && o.Tokens <= 0 && o.PromptTokens <= 0 && o.CompletionTokens <= 0 {
		return
	}
	k := s.tokens[purpose]
	split := max(o.PromptTokens, 0) + max(o.CompletionTokens, 0)
	k.prompt += uint64(max(o.PromptTokens, 0))
	k.completion += uint64(max(o.CompletionTokens, 0))
	if o.Tokens > split {
		k.unattributed += uint64(o.Tokens - split)
	}
	s.tokens[purpose] = k
}

// writeLLMTokens renders pg_sage_llm_tokens_total by client, database,
// purpose and kind (llm-04 added purpose).
func writeLLMTokens(b *strings.Builder, rows []llmHealthRow) {
	const name = "pg_sage_llm_tokens_total"
	metricsreg.Header(b, name)
	for _, row := range rows {
		for _, purpose := range slices.Sorted(maps.Keys(row.tokens)) {
			k := row.tokens[purpose]
			labels := withLabel(row.key.labels(), "purpose", purpose)
			for _, kind := range []struct {
				name string
				n    uint64
			}{{"prompt", k.prompt}, {"completion", k.completion},
				{"unattributed", k.unattributed}} {
				writeSample(b, name, withLabel(labels, "kind", kind.name), kind.n)
			}
		}
	}
	b.WriteString("\n")
}
