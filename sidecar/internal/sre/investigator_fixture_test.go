package sre

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Fixtures for the tool-calling investigator (roadmap 2.1): a coordinator
// with the investigator on, scripted tool-call replies of the fake
// OpenAI-compatible model, and readers of what the run stored.

type invOptions struct {
	auth    RootAuthority
	config  InvestigatorConfig
	timeout time.Duration
}

func investigatorCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	runner ProbeRunner, model *llm.Client, o invOptions) (*Coordinator, *logLines) {
	t.Helper()
	logs := &logLines{}
	cfg := DefaultCoordinatorConfig("test:" + string(NewUUID()))
	if o.timeout > 0 {
		cfg.ModelTimeout = o.timeout
	}
	icfg := o.config
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: cfg,
		Model: model, Notices: &OnceLog{}, LogFn: logs.logFn, RootAuthority: o.auth,
		Investigator: &icfg})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c, logs
}

// call answers with one native call of a (possibly undeclared) tool.
func call(name, args string) fakeReply { return callTool(name, fixed(args)) }

// calls answers with several native tool calls in one turn.
func calls(pairs ...string) fakeReply {
	return func(w http.ResponseWriter, _ string) {
		var tc []map[string]any
		for i := 0; i+1 < len(pairs); i += 2 {
			tc = append(tc, map[string]any{"id": "call_" + itoa(int64(i)), "type": "function",
				"function": map[string]any{"name": pairs[i], "arguments": pairs[i+1]}})
		}
		writeCompletion(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": tc})
	}
}

func probeArgs(id probes.ID) string { return `{"probe":"` + string(id) + `","args":{}}` }

type invClaim = wireClaim

type invFinal struct {
	Outcome string          `json:"outcome"`
	Root    string          `json:"root,omitempty"`
	Cause   *UnmodeledCause `json:"cause,omitempty"`
	Claims  []invClaim      `json:"claims"`
}

func (f invFinal) json() string {
	if f.Claims == nil {
		f.Claims = []invClaim{}
	}
	raw, _ := json.Marshal(f)
	return string(raw)
}

// submit answers with the final tool call built from the request.
func submit(build func(body string) invFinal) fakeReply {
	return callTool(ToolSubmit, func(body string) string { return build(body).json() })
}

func submitFixed(f invFinal) fakeReply { return submit(func(string) invFinal { return f }) }

// openUnproven finds the first unproven hypothesis the prompt lists.
func openUnproven(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`- ([a-z_]+) \[(unproven|alternative)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the prompt lists no unproven hypothesis: %s", body)
	}
	return m[1]
}

// transcriptOf returns the stored investigator run, failing without one.
func transcriptOf(t *testing.T, inv Investigation) *InvestigatorRun {
	t.Helper()
	if inv.Summary.Investigator == nil {
		t.Fatalf("no investigator transcript stored: %+v", inv.Summary)
	}
	return inv.Summary.Investigator
}

func stepsOf(run *InvestigatorRun, tool string) []InvestigatorStep {
	var out []InvestigatorStep
	for _, s := range run.Steps {
		if s.Tool == tool {
			out = append(out, s)
		}
	}
	return out
}

// evidenceByID maps an investigation's stored evidence by id.
func evidenceByID(t *testing.T, st *PostgresStore, inv Investigation) map[UUID]Evidence {
	t.Helper()
	ev, err := st.Evidence(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	out := map[UUID]Evidence{}
	for _, e := range ev {
		out[e.ID] = e
	}
	return out
}

func digestOf(e Evidence) string { return hex.EncodeToString(e.SHA256) }

// reservationsOf lists one investigation's model reservations by caller.
func reservationsOf(t *testing.T, st *PostgresStore, inv Investigation) map[string]int {
	t.Helper()
	rows, err := st.pool.Query(t.Context(), `SELECT caller_kind, count(*)
		FROM sage.sre_budget_reservations
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		GROUP BY caller_kind`, string(inv.Scope.DeploymentID),
		string(inv.Scope.DatabaseID), string(inv.ID))
	if err != nil {
		t.Fatalf("reservations: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[kind] = n
	}
	return out
}

// runnerCalls lists the probe ids a scripted runner received.
func runnerCalls(r *scriptedRunner) []probes.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []probes.ID
	for id, n := range r.calls {
		for range n {
			out = append(out, id)
		}
	}
	return out
}

func assertOnlyCatalogProbes(t *testing.T, r *scriptedRunner) {
	t.Helper()
	for _, id := range runnerCalls(r) {
		if _, ok := probes.Catalog().Spec(id); !ok {
			t.Fatalf("the runner was asked for %q, which is not a catalog probe", id)
		}
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// longTxRunner adds a long_transactions row for the idle holder.
func longTxRunner() *scriptedRunner {
	r := idleChainRunner()
	r.script(probes.LongTransactions, rows(probes.LongTransactions, probes.Row{
		"pid": int64(4242), "state": "idle in transaction", "xact_age_s": 95.0,
		"state_age_s": 80.0, "waiting": false, "backend_xmin_age": int64(12)}))
	return r
}
