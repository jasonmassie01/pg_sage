//go:build e2e

// ci-07: the default product path, with LLM features on once an endpoint
// and key are configured, driven end to end through the compiled binary.
// Every other binary test runs llm.enabled=false and the live TestLLM*
// tests need a real key, so config loading, the default LLM wiring, the
// advisor cycle, the scheduled briefing, the budget, the breaker and the
// fleet isolation of prompts were never exercised together. Each scenario
// gets its own database and its own scripted stub (internal/testsupport/
// llmstub) and runs in parallel with the others.
//
// Run with: go test ./e2e -tags=e2e -count=1 -run TestLLMDefaultPath
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/testsupport/llmstub"
)

// Prompt markers: text that only the named caller's prompt contains.
const (
	connMarker  = "PostgreSQL connection management expert"
	walMarker   = "PostgreSQL WAL and checkpoint tuning expert"
	briefMarker = "Write a concise health briefing"
)

// Stub answers. Each rationale carries a marker, so a finding whose
// recommendation holds it was written by the LLM (provenance).
const (
	connBody = `[{"object_identifier":"instance","severity":"info",` +
		`"rationale":"STUB-CONN: bound idle-in-transaction sessions",` +
		`"recommended_sql":"ALTER SYSTEM SET idle_in_transaction_session_timeout = '300s'"}]`
	walBody = `[{"object_identifier":"instance","severity":"info",` +
		`"rationale":"STUB-WAL: spread checkpoints further apart",` +
		`"recommended_sql":"ALTER SYSTEM SET checkpoint_timeout = '15min'"}]`
	stubBriefing = "STUB-BRIEFING: no urgent issues; review the open findings."
)

func TestLLMDefaultPath(t *testing.T) {
	admin := rawPipelinePool(t)
	bin := buildBinary(t)
	scenarios := []struct {
		name string
		run  func(*testing.T, *pgxpool.Pool, string)
	}{
		{"happy_path", testLLMHappyPath},
		{"parse_tolerance", testLLMParseTolerance},
		{"empty_completion", testLLMEmptyCompletion},
		{"rate_limited", testLLMRateLimited},
		{"timeout", testLLMTimeout},
		{"token_budget", testLLMTokenBudget},
		{"fleet_isolation", testLLMFleetIsolation},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, admin, bin)
		})
	}
}

// Happy path: within the first cycles the advisor turns valid and fenced
// JSON into findings carrying the stub's text, a briefing is produced by
// the LLM, the rules floor still fires, and spend is visible in the API
// and in Prometheus.
func testLLMHappyPath(t *testing.T, admin *pgxpool.Pool, bin string) {
	r := startLLMRun(t, admin, bin, "happy", llmstub.Options{Rules: []llmstub.Rule{
		{Match: connMarker, Mode: llmstub.ModeValid, Body: connBody},
		{Match: walMarker, Mode: llmstub.ModeFencedJSON, Body: walBody},
		{Match: briefMarker, Mode: llmstub.ModeValid, Body: stubBriefing},
	}}, "")
	r.waitFor("LLM advisor findings, an LLM briefing and the rules floor",
		func() (bool, string) {
			f := r.findings()
			briefs := quickScalar(r.pool, `SELECT count(*)::text FROM sage.briefings
				WHERE llm_used AND content_text LIKE '%STUB-BRIEFING%'`)
			ok := hasRecommendation(f["connection_tuning"], "STUB-CONN") &&
				hasRecommendation(f["wal_tuning"], "STUB-WAL") &&
				len(f["sequence_exhaustion"]) > 0 && briefs != "0" &&
				!strings.HasPrefix(briefs, "<err")
			return ok, fmt.Sprintf("categories=%v llm_briefings=%s", categoryNames(f), briefs)
		})
	r.assertStubRequestsShaped()
	for _, marker := range []string{connMarker, walMarker, briefMarker} {
		if r.stub.CountMatching(marker) == 0 {
			t.Errorf("no stub request carried %q", marker)
		}
	}
	st := r.llmStatus()["general"]
	if !st.Enabled || st.CircuitOpen || st.TokensUsed < 150 {
		t.Errorf("llm status general = %+v, want enabled, closed, >= 150 tokens "+
			"(three answered calls at 50)", st)
	}
	tokens, _ := r.metric(generalLLMSeries("pg_sage_llm_tokens_used_today"))
	enabled, _ := r.metric(generalLLMSeries("pg_sage_llm_enabled"))
	circuit, okCircuit := r.metric(generalLLMSeries("pg_sage_llm_circuit_open"))
	if tokens < 150 || enabled != 1 || circuit != 0 || !okCircuit {
		t.Errorf("prometheus tokens=%v enabled=%v circuit=%v(present=%v), "+
			"want >=150, 1, 0", tokens, enabled, circuit, okCircuit)
	}
}

// Invalid input: prose-wrapped JSON is parsed into a finding; a reply with
// no JSON is rejected in the log and produces no finding.
func testLLMParseTolerance(t *testing.T, admin *pgxpool.Pool, bin string) {
	r := startLLMRun(t, admin, bin, "parse", llmstub.Options{Rules: []llmstub.Rule{
		{Match: connMarker, Mode: llmstub.ModeProseJSON, Body: connBody},
		{Match: walMarker, Mode: llmstub.ModeNonJSON, Body: walBody},
		{Match: briefMarker, Mode: llmstub.ModeValid, Body: stubBriefing},
	}}, "")
	r.waitFor("a finding from prose-wrapped JSON and a logged non-JSON rejection",
		func() (bool, string) {
			f := r.findings()
			rejected := strings.Contains(r.logs(), "advisor: wal_tuning: parse error")
			ok := hasRecommendation(f["connection_tuning"], "STUB-CONN") &&
				r.stub.CountMatching(walMarker) > 0 && rejected
			return ok, fmt.Sprintf("categories=%v wal_rejection_logged=%v",
				categoryNames(f), rejected)
		})
	time.Sleep(llmAnalyzerCycle + 2*time.Second)
	if n := len(r.findings()["wal_tuning"]); n != 0 {
		t.Errorf("non-JSON WAL reply produced %d wal_tuning finding(s), want 0", n)
	}
}

// Nil/empty: a blank completion is a failed call: no advisor finding, the
// briefing falls back to the deterministic text, and the breaker stays
// closed (a blank reply is not a provider outage).
func testLLMEmptyCompletion(t *testing.T, admin *pgxpool.Pool, bin string) {
	r := startLLMRun(t, admin, bin, "empty",
		llmstub.Options{Default: llmstub.ModeEmpty}, "")
	r.waitFor("empty replies to both sub-advisors and a fallback briefing",
		func() (bool, string) {
			f := r.findings()
			fallback := quickScalar(r.pool,
				`SELECT count(*)::text FROM sage.briefings WHERE NOT llm_used`)
			ok := r.stub.CountMatching(connMarker) > 0 &&
				r.stub.CountMatching(walMarker) > 0 &&
				len(f["sequence_exhaustion"]) > 0 && fallback != "0" &&
				!strings.HasPrefix(fallback, "<err")
			return ok, fmt.Sprintf("categories=%v fallback_briefings=%s",
				categoryNames(f), fallback)
		})
	time.Sleep(llmAnalyzerCycle + 2*time.Second)
	r.assertNoLLMFindings("empty completion")
	if n := quickScalar(r.pool, `SELECT count(*)::text FROM sage.briefings
		WHERE llm_used`); n != "0" {
		t.Errorf("%s briefing(s) marked llm_used from an empty completion, want 0", n)
	}
	logs := r.logs()
	if !strings.Contains(logs, "advisor: connections:") ||
		!strings.Contains(logs, "empty response") {
		t.Errorf("empty completion was not logged as a failed advisor call")
	}
	if st := r.llmStatus()["general"]; st.CircuitOpen {
		t.Errorf("breaker opened on empty completions: %+v", st)
	}
}

// Error propagation: every request is answered 429. The stub also sends
// Retry-After: 1, which the client does not honour yet (llm-02 owns that):
// it retries 429 on a fixed 1/4/16 s ladder, so one failed call is four
// requests and about 21 s, and the breaker opens after three failed calls.
// The first-look summary, the advisor's sequential sub-advisors, the
// briefing and the incident narrator all spend through the general client
// at once, so three failures land well inside llmPathDeadline (about 25 s
// locally; waitFor logs the time). The API and Prometheus both show the
// open breaker, and the rules-only findings still appear.
func testLLMRateLimited(t *testing.T, admin *pgxpool.Pool, bin string) {
	r := startLLMRun(t, admin, bin, "ratelimit", llmstub.Options{
		Default: llmstub.ModeRateLimited, RetryAfterSeconds: 1,
	}, "")
	assertProviderFailureSurfaces(t, r, "rate limiting")
	// A 429 is retried before the call counts as failed.
	if n := r.stub.Count(); n < 4 {
		t.Errorf("stub saw %d requests, want >= 4 (one call and its three retries)", n)
	}
}

// Error propagation: a provider that never answers within
// llm.timeout_seconds opens the breaker the same way.
func testLLMTimeout(t *testing.T, admin *pgxpool.Pool, bin string) {
	r := startLLMRun(t, admin, bin, "timeout", llmstub.Options{
		Default: llmstub.ModeTimeout, Delay: 30 * time.Second,
	}, "  timeout_seconds: 1\n")
	assertProviderFailureSurfaces(t, r, "timeouts")
}

func assertProviderFailureSurfaces(t *testing.T, r *llmRun, cause string) {
	t.Helper()
	r.waitFor("an open breaker in the API and metrics, and the rules floor",
		func() (bool, string) {
			open, detail := r.circuitOpen()
			f := r.findings()
			return open && len(f["sequence_exhaustion"]) > 0,
				fmt.Sprintf("%s categories=%v", detail, categoryNames(f))
		})
	r.assertNoLLMFindings(cause)
	st := r.llmStatus()["general"]
	tokens, _ := r.metric(generalLLMSeries("pg_sage_llm_tokens_used_today"))
	if st.TokensUsed != 0 || tokens != 0 {
		t.Errorf("%s: tokens charged without an answer: status=%d gauge=%v",
			cause, st.TokensUsed, tokens)
	}
}

// Boundary: with llm.token_budget_daily = N, no call starts once
// tokens_used_today >= N. A call reserves its max_tokens before any I/O
// (4096 for the advisor, llm.context_budget_tokens = 8192 for the
// briefing) and the stub reports budgetAnswer tokens per answer. N = 9000
// admits every caller's reservation at zero spend, so once the first
// answer spends past N a refusal can only come from the tokens already
// used. The per-minute briefing is the deterministic post-cap caller: its
// logged refusal proves a caller ran after the cap and was refused (a
// budget below 8192 refused it even at zero spend, so the freeze proved
// nothing). optimizer_llm is off by default, so the general client is the
// only budget every caller spends through. The exact-cap assertion
// tightens when llm-03 lands.
func testLLMTokenBudget(t *testing.T, admin *pgxpool.Pool, bin string) {
	const budget, budgetAnswer = 9000, 10000
	r := startLLMRun(t, admin, bin, "budget", llmstub.Options{
		TotalTokens: budgetAnswer,
		Rules: []llmstub.Rule{
			{Match: connMarker, Mode: llmstub.ModeValid, Body: connBody},
			{Match: walMarker, Mode: llmstub.ModeValid, Body: walBody},
			{Match: briefMarker, Mode: llmstub.ModeValid, Body: stubBriefing},
		},
	}, fmt.Sprintf("  token_budget_daily: %d\n", budget))
	r.waitFor("tokens_used_today >= the daily budget", func() (bool, string) {
		used, ok := r.metric(generalLLMSeries("pg_sage_llm_tokens_used_today"))
		return ok && used >= budget, fmt.Sprintf("tokens_used_today=%v", used)
	})
	atCap, refusedAtCap := r.stub.Count(), r.briefingBudgetRefusals()
	// Three analyzer cycles and the next scheduled briefing minute.
	time.Sleep(3*llmAnalyzerCycle + 62*time.Second)
	if after := r.stub.Count(); after != atCap {
		t.Errorf("stub requests grew from %d to %d after the budget was spent: %s",
			atCap, after, r.stubSummary())
	}
	if atCap < 1 {
		t.Errorf("no call reached the stub before the cap (count %d)", atCap)
	}
	if refused := r.briefingBudgetRefusals(); refused <= refusedAtCap {
		t.Errorf("no briefing was refused by the budget after the cap "+
			"(%d refusals at the cap, %d after): the freeze proves nothing",
			refusedAtCap, refused)
	}
	assertSingleGeneralBudget(t, r.llmStatus(), budget)
}

// briefingBudgetRefusals counts the briefing's logged budget refusals.
func (r *llmRun) briefingBudgetRefusals() int {
	n := 0
	for _, line := range strings.Split(r.logs(), "\n") {
		if strings.Contains(line, "briefing: LLM enhancement failed:") &&
			strings.Contains(line, "budget exhausted") {
			n++
		}
	}
	return n
}

// assertSingleGeneralBudget checks the status lists only the general
// client (no separate optimizer budget on the default config), exhausted.
func assertSingleGeneralBudget(t *testing.T, clients map[string]llm.ClientStatus,
	budget int) {
	t.Helper()
	st, ok := clients["general"]
	if len(clients) != 1 || !ok {
		t.Fatalf("llm status clients = %v, want only general: every caller must "+
			"spend through the one budget under test", clients)
	}
	if !st.Exhausted || st.TokensUsed < int64(budget) || st.TokenBudget != budget {
		t.Errorf("llm status general = %+v, want exhausted with >= %d of %d",
			st, budget, budget)
	}
}

// Concurrency (fleet, two databases): each database's prompts name only
// that database's objects, and its findings only its own objects. The wait
// covers each database's first-look summary and its briefing, which is
// built from that database's findings (a fleet-context leak shows there).
func testLLMFleetIsolation(t *testing.T, admin *pgxpool.Pool, bin string) {
	stub := llmstub.New(t, llmstub.Options{Rules: []llmstub.Rule{
		{Match: briefMarker, Mode: llmstub.ModeValid, Body: stubBriefing},
	}})
	dbA, dbB := llmScenarioName("fleet_a"), llmScenarioName("fleet_b")
	seqA, seqB := "llmseq_fleet_a", "llmseq_fleet_b"
	_, poolA := llmScenarioDB(t, admin, dbA, seqA)
	_, poolB := llmScenarioDB(t, admin, dbB, seqB)
	apiPort, promPort := freePort(t), freePort(t)
	cfgPath := writeLLMFleetConfig(t, stub.URL, dbA, dbB, apiPort, promPort)
	env := newLLMEnv(bin, cfgPath, apiPort, promPort)
	launchLLMBinary(t, env, "fleet", "")
	r := &llmRun{t: t, env: env, stub: stub, pool: poolA}
	r.waitFor("LLM prompts and briefings about each fleet database",
		func() (bool, string) {
			a, b := stub.CountMatching(seqA), stub.CountMatching(seqB)
			briefA := countMatchingAll(stub, briefMarker, seqA)
			briefB := countMatchingAll(stub, briefMarker, seqB)
			return a > 0 && b > 0 && briefA > 0 && briefB > 0,
				fmt.Sprintf("prompts naming A=%d B=%d, briefings naming A=%d B=%d",
					a, b, briefA, briefB)
		})
	for i, req := range stub.Requests() {
		p := req.Prompt()
		namesA := strings.Contains(p, seqA) || strings.Contains(p, dbA)
		namesB := strings.Contains(p, seqB) || strings.Contains(p, dbB)
		if namesA && namesB {
			t.Errorf("stub request %d names both fleet databases: %.400s", i, p)
		}
	}
	assertFindingsOnlyName(t, poolA, dbA, seqB)
	assertFindingsOnlyName(t, poolB, dbB, seqA)
}

// countMatchingAll counts stub requests whose prompt holds every text.
func countMatchingAll(stub *llmstub.Server, texts ...string) int {
	n := 0
	for _, req := range stub.Requests() {
		p, all := req.Prompt(), true
		for _, text := range texts {
			all = all && strings.Contains(p, text)
		}
		if all {
			n++
		}
	}
	return n
}

// assertFindingsOnlyName fails if a database's findings mention the other
// database's sequence.
func assertFindingsOnlyName(t *testing.T, pool *pgxpool.Pool, db, foreign string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE object_identifier LIKE '%' || $1 || '%'
		   OR recommendation LIKE '%' || $1 || '%'`, foreign).Scan(&n)
	if err != nil {
		t.Fatalf("%s: count foreign findings: %v", db, err)
	}
	if n != 0 {
		t.Errorf("%s: %d finding(s) name %s from the other database", db, n, foreign)
	}
}
