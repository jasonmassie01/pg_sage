package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M3 wiring: the model turn is on by default (sre.llm.enabled)
// and used whenever an LLM is configured. Without one the investigator
// stays deterministic and says why once per process; enabled: false
// makes no model call at all.

type capturedLogs struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLogs) logFn(level, msg string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (c *capturedLogs) count(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func testLLMClient(endpoint string, enabled bool) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: enabled, Endpoint: endpoint, APIKey: "k",
		Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 500000},
		func(string, string, ...any) {})
}

func TestSREModelClient_DefaultsAndNotices(t *testing.T) {
	on := config.DefaultConfig().SRE
	if !on.LLM.Enabled {
		t.Fatal("sre.llm.enabled must default to true")
	}
	off := on
	off.LLM.Enabled = false
	configured := testLLMClient("http://127.0.0.1:1", true)
	logs, notices := &capturedLogs{}, &sre.OnceLog{}
	if got := sreModelClient(on, configured, notices, logs.logFn); got != configured {
		t.Fatalf("default config with a provider = %v, want the client", got)
	}
	if got := sreModelClient(off, configured, notices, logs.logFn); got != nil {
		t.Fatalf("enabled: false = %v, want no model", got)
	}
	for _, client := range []*llm.Client{nil, testLLMClient("", false), nil} {
		if got := sreModelClient(on, client, notices, logs.logFn); got != nil {
			t.Fatalf("no provider = %v, want no model", got)
		}
	}
	if n := logs.count("model turn unavailable"); n != 1 {
		t.Fatalf("notices = %d (%v), want exactly one per process", n, logs.lines)
	}
	if logs.count("llm.enabled") != 1 {
		t.Fatalf("the notice must say how to configure an LLM: %v", logs.lines)
	}
}

func TestSRELimits_DailyAllocationFollowsTheModel(t *testing.T) {
	client := testLLMClient("http://127.0.0.1:1", true)
	logs, notices := &capturedLogs{}, &sre.OnceLog{}
	none := sreLimits(nil, 500000, notices, logs.logFn)
	if none.DatabaseDailyTokens != 0 || none.DeploymentDailyTokens != 0 {
		t.Fatalf("no model must get no allocation: %+v", none)
	}
	with := sreLimits(client, 500000, notices, logs.logFn)
	if with.DatabaseDailyTokens != 500000 || with.DeploymentDailyTokens != 500000 ||
		with.Validate() != nil {
		t.Fatalf("limits with a model = %+v (%v)", with, with.Validate())
	}
	zero := sreLimits(client, 0, notices, logs.logFn)
	if zero.DatabaseDailyTokens != 0 || zero.Validate() != nil ||
		logs.count("token_budget_daily") != 1 {
		t.Fatalf("a zero daily budget: %+v, logs %v", zero, logs.lines)
	}
	if def := sre.DefaultLimits(); with.MaxModelTurns != def.MaxModelTurns ||
		with.MaxInputTokens != def.MaxInputTokens || with.MaxProbes != def.MaxProbes {
		t.Fatalf("model limits changed the R1 ceilings: %+v", with)
	}
}

// chainRunner answers the lock plan with an idle-in-transaction holder.
type chainRunner struct{}

func (chainRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	if id == probes.LockGraph {
		res.Status = probes.StatusOK
		res.Rows = []probes.Row{{"waiter_pid": int64(20), "lock_type": "relation",
			"requested_mode": "AccessExclusiveLock", "relation": "public.orders",
			"blocker_pid": int64(4242), "blocker_kind": "backend",
			"blocker_state": "idle in transaction", "blocker_waiting": false,
			"blocker_xact_age_s": 90.0, "blocker_backend_start": time.Now().UTC()}}
	}
	return res
}

func countingModel(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"message": map[string]any{"role": "assistant",
				"content": `{"ranking":["idle_in_tx_holder","ddl_lock_queue"],"claims":[]}`},
			"finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 200}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func investigateWith(t *testing.T, settings config.SREConfig,
	client *llm.Client) sre.Investigation {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: chainRunner{}, name: "m3_db", settings: settings, llm: client,
		dailyTokens: 500000, notices: &sre.OnceLog{},
		runtimeKey: fmt.Sprintf("m3:%d", time.Now().UnixNano()),
		logFn:      func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	inv, _, err := svc.Coordinator().Start(ctx, sre.Trigger{CaseID: "case:m3",
		Kind: sre.TriggerLock, Subject: "incident m3", IdempotencyKey: "m3"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Coordinator().Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	d, err := svc.Detail(ctx, inv.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	return d.Investigation
}

func TestSREInvestigator_ModelTurnOnByDefault(t *testing.T) {
	srv, calls := countingModel(t)
	inv := investigateWith(t, config.DefaultConfig().SRE, testLLMClient(srv.URL, true))
	if inv.Summary.Root != "idle_in_tx_holder" || inv.Summary.ModelRanking == nil ||
		inv.ModelTurns != 1 || calls.Load() != 1 {
		t.Fatalf("default config: root %q ranking %+v turns %d calls %d",
			inv.Summary.Root, inv.Summary.ModelRanking, inv.ModelTurns, calls.Load())
	}
}

func TestSREInvestigator_ExplicitlyOffMakesNoModelCall(t *testing.T) {
	srv, calls := countingModel(t)
	settings := config.DefaultConfig().SRE
	settings.LLM.Enabled = false
	inv := investigateWith(t, settings, testLLMClient(srv.URL, true))
	if inv.Summary.Root != "idle_in_tx_holder" || inv.Summary.ModelRanking != nil ||
		inv.ModelTurns != 0 || calls.Load() != 0 {
		t.Fatalf("enabled: false: root %q ranking %+v turns %d calls %d",
			inv.Summary.Root, inv.Summary.ModelRanking, inv.ModelTurns, calls.Load())
	}
}

func TestSREInvestigator_NoProviderRunsDeterministically(t *testing.T) {
	inv := investigateWith(t, config.DefaultConfig().SRE, nil)
	if inv.State != sre.StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.Summary.ModelRanking != nil || inv.ModelTurns != 0 {
		t.Fatalf("no provider: %+v", inv)
	}
}
