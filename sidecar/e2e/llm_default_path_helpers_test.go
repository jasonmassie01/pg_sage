//go:build e2e

// Helpers for llm_default_path_test.go: one fresh database, one scripted
// LLM stub and one real sidecar binary per scenario.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/testsupport/llmstub"
)

const (
	// llmPathDeadline bounds each scenario's wait: startup, two analyzer
	// cycles and the first scheduled briefing (at the next minute).
	llmPathDeadline = 170 * time.Second
	llmPathPoll     = 2 * time.Second
	// llmAnalyzerCycle is analyzer.interval_seconds in the scenario config.
	llmAnalyzerCycle = 6 * time.Second
	llmStubKey       = "stub-key"
	llmStubModel     = "stub-model"
)

// llmRun is one binary run against one scenario database and one stub.
type llmRun struct {
	t    *testing.T
	env  *testEnv
	stub *llmstub.Server
	pool *pgxpool.Pool
}

// llmScenarioDB creates a fresh database, seeds the rules-only floor (a
// sequence near exhaustion) and returns its DSN and a pool on it.
func llmScenarioDB(
	t *testing.T, admin *pgxpool.Pool, name, sequence string,
) (string, *pgxpool.Pool) {
	t.Helper()
	createTestDB(t, admin, name)
	t.Cleanup(func() { dropTestDB(t, admin, name) })
	dsn := dbDSN(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect scenario database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	mustExec(t, pool, fmt.Sprintf(
		"CREATE SEQUENCE %s AS smallint; SELECT setval('%s', 26000);",
		sequence, sequence))
	// Optional: the slow-query collector needs it preloaded; the LLM
	// path under test does not.
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Logf("pg_stat_statements unavailable in %s (continuing): %v", name, err)
	}
	return dsn, pool
}

// llmScenarioName is a database name unique to this process and scenario.
func llmScenarioName(scenario string) string {
	return fmt.Sprintf("sage_llm_%s_%d_%d", scenario, os.Getpid(),
		time.Now().UnixNano()%1_000_000)
}

// writeLLMDefaultConfig renders the documented default config: nothing set
// but the target, fast cycles, a briefing every minute and an LLM endpoint
// with a key. llm.enabled is left at its default (on). extraLLM holds more
// llm: keys, each indented two spaces.
func writeLLMDefaultConfig(
	t *testing.T, dsn, endpoint, extraLLM string, apiPort, promPort int,
) string {
	t.Helper()
	yaml := fmt.Sprintf(`mode: standalone

postgres:
  database_url: %q
  max_connections: 4

collector:
  interval_seconds: 5

analyzer:
  interval_seconds: %d

llm:
  endpoint: %q
  api_key: %q
  model: %q
%s
briefing:
  schedule: "* * * * *"

prometheus:
  listen_addr: "127.0.0.1:%d"

api:
  listen_addr: "127.0.0.1:%d"
`, dsn, int(llmAnalyzerCycle/time.Second), endpoint, llmStubKey, llmStubModel,
		extraLLM, promPort, apiPort)
	path := filepath.Join(t.TempDir(), "llm-default-config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write LLM default config: %v", err)
	}
	return path
}

// startLLMRun starts a stub, a fresh database and the binary on the
// documented default config, and waits until the API accepts a login.
func startLLMRun(
	t *testing.T, admin *pgxpool.Pool, bin, scenario string,
	opts llmstub.Options, extraLLM string,
) *llmRun {
	t.Helper()
	stub := llmstub.New(t, opts)
	dsn, pool := llmScenarioDB(t, admin, llmScenarioName(scenario),
		"llmseq_"+scenario)
	apiPort, promPort := freePort(t), freePort(t)
	cfgPath := writeLLMDefaultConfig(t, dsn, stub.URL, extraLLM, apiPort, promPort)
	env := newLLMEnv(bin, cfgPath, apiPort, promPort)
	launchLLMBinary(t, env, "standalone", dsn)
	return &llmRun{t: t, env: env, stub: stub, pool: pool}
}

func newLLMEnv(bin, cfgPath string, apiPort, promPort int) *testEnv {
	return &testEnv{
		binaryPath: bin, apiPort: apiPort, promPort: promPort,
		apiBase:    fmt.Sprintf("http://127.0.0.1:%d", apiPort),
		promBase:   fmt.Sprintf("http://127.0.0.1:%d", promPort),
		configPath: cfgPath,
	}
}

// launchLLMBinary starts the binary with the stub's key in the
// environment (never a real one) and waits for the logged-in API.
func launchLLMBinary(t *testing.T, env *testEnv, mode, dsn string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	cmd := exec.CommandContext(ctx, env.binaryPath, "--config", env.configPath)
	cmd.Env = append(os.Environ(),
		"SAGE_DATABASE_URL="+dsn,
		"SAGE_MODE="+mode,
		"SAGE_LLM_API_KEY="+llmStubKey,
		"LLM_API_KEY="+llmStubKey,
		"SAGE_META_DB=",
	)
	if directory := os.Getenv("SAGE_E2E_COVERAGE_DIR"); directory != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+directory)
	}
	env.stdout, env.stderr = &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = env.stdout, env.stderr
	env.cmd = cmd
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start binary (%s): %v", mode, err)
	}
	t.Cleanup(func() {
		stopBinary(t, env)
		if t.Failed() {
			t.Logf("--- binary log tail ---\n%s", tail(env.stderr.String(), 8000))
		}
	})
	waitReady(t, env)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// waitFor polls cond until it holds or llmPathDeadline passes, and logs
// how long it took (the CI evidence for the scenario's timing budget).
func (r *llmRun) waitFor(what string, cond func() (bool, string)) {
	r.t.Helper()
	start := time.Now()
	deadline := start.Add(llmPathDeadline)
	detail := ""
	for time.Now().Before(deadline) {
		var ok bool
		if ok, detail = cond(); ok {
			r.t.Logf("%s after %s: %s (stub requests: %s)", what,
				time.Since(start).Round(time.Second), detail, r.stubSummary())
			return
		}
		time.Sleep(llmPathPoll)
	}
	r.t.Fatalf("timed out after %s waiting for %s: %s (stub requests: %s)",
		llmPathDeadline, what, detail, r.stubSummary())
}

// stubSummary counts recorded requests by caller and answering mode.
func (r *llmRun) stubSummary() string {
	counts := map[string]int{}
	for _, req := range r.stub.Requests() {
		counts[callerOf(req)+"/"+string(req.Mode)]++
	}
	return fmt.Sprintf("%v", counts)
}

// callerOf names the sidecar caller of a request from its prompt marker.
func callerOf(req llmstub.Request) string {
	p := req.Prompt()
	for _, c := range []struct{ name, marker string }{
		{"connections", connMarker}, {"wal", walMarker}, {"briefing", briefMarker},
	} {
		if strings.Contains(p, c.marker) {
			return c.name
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(req.System), "\n")
	if len(first) > 60 {
		first = first[:60]
	}
	return "other(" + first + ")"
}

// logs is everything the binary wrote so far.
func (r *llmRun) logs() string { return r.env.stderr.String() + r.env.stdout.String() }

// findings returns the open findings the API lists, grouped by category.
func (r *llmRun) findings() map[string][]map[string]any {
	r.t.Helper()
	code, body := httpGet(r.t, r.env, r.env.apiBase+"/api/v1/findings")
	out := map[string][]map[string]any{}
	if code != http.StatusOK {
		return out
	}
	var resp struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		r.t.Fatalf("decode /api/v1/findings: %v: %.300s", err, body)
	}
	for _, f := range resp.Findings {
		category, _ := f["category"].(string)
		out[category] = append(out[category], f)
	}
	return out
}

// hasRecommendation reports whether any finding's recommendation text
// carries marker, i.e. was written by the stub (LLM provenance).
func hasRecommendation(rows []map[string]any, marker string) bool {
	for _, f := range rows {
		if text, _ := f["recommendation"].(string); strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func categoryNames(m map[string][]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// llmStatus decodes /api/v1/llm/status into its per-client entries.
func (r *llmRun) llmStatus() map[string]llm.ClientStatus {
	r.t.Helper()
	code, body := httpGet(r.t, r.env, r.env.apiBase+"/api/v1/llm/status")
	if code != http.StatusOK {
		r.t.Fatalf("/api/v1/llm/status: status %d: %.300s", code, body)
	}
	clients, err := decodeLLMClients([]byte(body))
	if err != nil {
		r.t.Fatalf("/api/v1/llm/status: %v: %.300s", err, body)
	}
	return clients
}

// errLLMNotWired is the status body of a process with no LLM runtime: the
// handler lists clients as an empty array, not a keyed object.
var errLLMNotWired = errors.New("LLM manager not wired: no LLM clients listed " +
	"on the default LLM-on config")

// decodeLLMClients reads the per-client map from a status body. A process
// without an LLM runtime answers clients:[]; that is reported as
// errLLMNotWired rather than as a JSON type error.
func decodeLLMClients(body []byte) (map[string]llm.ClientStatus, error) {
	var resp struct {
		Clients json.RawMessage `json:"clients"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode status body: %w", err)
	}
	raw := bytes.TrimSpace(resp.Clients)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("%w (clients=%s)", errLLMNotWired, raw)
	}
	clients := map[string]llm.ClientStatus{}
	if err := json.Unmarshal(raw, &clients); err != nil {
		return nil, fmt.Errorf("decode clients: %w", err)
	}
	return clients, nil
}

// generalLLMSeries is the standalone shared client's series of an LLM
// gauge; since observability-05 they are labelled per client.
func generalLLMSeries(name string) string {
	return name + `{client="general",database=""}`
}

// metric returns the Prometheus sample whose name and labels are exactly
// name (a bare name for an unlabelled one); ok is false if absent.
func (r *llmRun) metric(name string) (float64, bool) {
	r.t.Helper()
	code, body := httpGet(r.t, r.env, r.env.promBase+"/metrics")
	if code != http.StatusOK {
		r.t.Fatalf("prometheus /metrics: status %d", code)
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[len(name):]), 64)
		if err != nil {
			r.t.Fatalf("metric %s: unparsable sample %q: %v", name, line, err)
		}
		return v, true
	}
	return 0, false
}

// circuitOpen reports whether the API and Prometheus both show the
// general client's breaker open.
func (r *llmRun) circuitOpen() (bool, string) {
	st := r.llmStatus()["general"]
	gauge, ok := r.metric(generalLLMSeries("pg_sage_llm_circuit_open"))
	return st.CircuitOpen && ok && gauge == 1,
		fmt.Sprintf("status.circuit_open=%v gauge=%v(present=%v)", st.CircuitOpen, gauge, ok)
}

// assertNoLLMFindings fails if any advisor category reached the API.
func (r *llmRun) assertNoLLMFindings(why string) {
	r.t.Helper()
	f := r.findings()
	for _, category := range []string{"connection_tuning", "wal_tuning"} {
		if n := len(f[category]); n != 0 {
			r.t.Errorf("%s: %d %s finding(s) listed, want none: %v",
				why, n, category, f[category])
		}
	}
}

// assertStubRequestsShaped checks every request carried the configured
// key and model to the chat-completions path.
func (r *llmRun) assertStubRequestsShaped() {
	r.t.Helper()
	for _, req := range r.stub.Requests() {
		if req.Authorization != "Bearer "+llmStubKey || req.Model != llmStubModel ||
			req.Path != "/v1/chat/completions" {
			r.t.Fatalf("stub request not shaped by the config: path=%s auth=%q model=%q",
				req.Path, req.Authorization, req.Model)
		}
	}
}

// writeLLMFleetConfig renders a two-database fleet config on the same
// defaults as writeLLMDefaultConfig.
func writeLLMFleetConfig(
	t *testing.T, endpoint, dbA, dbB string, apiPort, promPort int,
) string {
	t.Helper()
	base, err := pgxpool.ParseConfig(adminDSN(t))
	if err != nil {
		t.Fatalf("parse designated fleet DSN: %v", err)
	}
	c := base.ConnConfig
	entry := func(name string) string {
		return fmt.Sprintf("  - name: %s\n    host: %q\n    port: %d\n    user: %q\n"+
			"    password: %q\n    database: %s\n    sslmode: disable\n"+
			"    max_connections: 3\n", name, c.Host, c.Port, c.User, c.Password, name)
	}
	yaml := fmt.Sprintf(`mode: fleet

databases:
%s%s
collector:
  interval_seconds: 5

analyzer:
  interval_seconds: %d

llm:
  endpoint: %q
  api_key: %q
  model: %q

briefing:
  schedule: "* * * * *"

prometheus:
  listen_addr: "127.0.0.1:%d"

api:
  listen_addr: "127.0.0.1:%d"
`, entry(dbA), entry(dbB), int(llmAnalyzerCycle/time.Second), endpoint,
		llmStubKey, llmStubModel, promPort, apiPort)
	path := filepath.Join(t.TempDir(), "llm-fleet-config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write LLM fleet config: %v", err)
	}
	return path
}
