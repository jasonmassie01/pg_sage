package lakebaselive

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// All native SRE tests require the existing exact endpoint/database/TLS guard
// plus a separate opt-in. They never enable background workers or actions.
func sreNativePool(t *testing.T) (*pgxpool.Pool, context.Context, *probes.ReadPolicy) {
	t.Helper()
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_SRE") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_SRE=1 for bounded database-scoped SRE acceptance")
	}
	p, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	policy := &probes.ReadPolicy{Version: 1, Scope: "database", Database: p.Config().ConnConfig.Database,
		Binding: probes.PoolBinding(p)}
	return p, ctx, policy
}

type sourceResult struct {
	ID        probes.ID     `json:"id"`
	Scope     string        `json:"scope"`
	Eligible  bool          `json:"eligible"`
	Executed  bool          `json:"sql_executed"`
	Status    probes.Status `json:"status"`
	Reason    string        `json:"reason,omitempty"`
	Version   string        `json:"probe_version"`
	Rows      int           `json:"rows"`
	ElapsedMS int64         `json:"elapsed_ms"`
}

func writeSREReport(t *testing.T, name string, b []byte) {
	t.Helper()
	if path := os.Getenv(name); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLakebaseSRESourceMatrix(t *testing.T) {
	p, ctx, policy := sreNativePool(t)
	ids := probes.Catalog().IDs()
	if len(ids) != 32 {
		t.Fatal("diagnostic inventory changed; review native acceptance")
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var pid int32
	var started time.Time
	if err := conn.QueryRow(ctx, "SELECT pid, backend_start FROM pg_stat_activity WHERE pid=pg_backend_pid()").Scan(&pid, &started); err != nil {
		t.Fatal(err)
	}
	runner := probes.NewRunner(p, probes.Catalog(), nil, policy)
	actions := probes.NewRunner(p, probes.ActionRegistry(), nil, policy)
	var results []sourceResult
	for _, id := range append(ids, probes.ActionRegistry().IDs()...) {
		args := probes.Args{}
		reg := probes.Catalog()
		if _, ok := probes.ActionRegistry().Spec(id); ok {
			reg = probes.ActionRegistry()
		}
		spec, _ := reg.Spec(id)
		if spec.Args == probes.ArgsBackend {
			args.PID, args.BackendStart = pid, started
		}
		if spec.Args == probes.ArgsWindow {
			args.Window = time.Minute
		}
		var res probes.Result
		if reg == probes.ActionRegistry() {
			res = actions.Run(ctx, id, args)
		} else {
			res = runner.Run(ctx, id, args)
		}
		results = append(results, sourceResult{ID: id, Scope: probes.SourceScope(id), Eligible: policy.Allows(id),
			Executed: res.Timing.Attempts > 0, Status: res.Status, Reason: res.Reason,
			Version: res.Version, Rows: len(res.Rows), ElapsedMS: res.ElapsedMS})
		if !policy.Allows(id) && (res.Reason != probes.ReasonPolicyDenied || res.Timing.Attempts != 0) {
			t.Errorf("denied source executed: %s", id)
		}
		if id == probes.StatDatabase || id == probes.BackendIdentity {
			if res.Status != probes.StatusOK {
				t.Errorf("required sensor %s = %s/%s", id, res.Status, res.Reason)
			}
		}
	}
	// Signals are independent adapters, not part of the 32 diagnostic catalog.
	for _, id := range []probes.ID{probes.ChangeFeed, probes.SLOStatus, probes.PoolerPools} {
		r := probes.PolicyDenied(id, "outside native fixture adapter scope")
		if policy.Allows(id) {
			r.Status, r.Reason = probes.StatusUnsupported, "source_not_configured"
		}
		results = append(results, sourceResult{ID: id, Scope: probes.SourceScope(id), Eligible: policy.Allows(id), Status: r.Status, Reason: r.Reason})
	}
	// A missing synthetic query identity is an explicit refusal, not an
	// observed/executed workload plan. The real tool loop is tested separately.
	explain := sre.NewStatementExplainer(p).ExplainStatement(probes.WithReadPolicy(ctx, policy), -9223372036854775807)
	results = append(results, sourceResult{ID: sre.ExplainProbeID, Scope: "database", Eligible: true,
		Status: explain.Status, Reason: explain.Reason, Version: explain.Version, Rows: len(explain.Rows), ElapsedMS: explain.ElapsedMS})
	report := struct {
		Database string         `json:"database"`
		Policy   string         `json:"policy_hash"`
		Sources  []sourceResult `json:"sources"`
	}{policy.Database, policy.Hash(), results}
	b, _ := json.MarshalIndent(report, "", "  ")
	t.Log(string(b))
	writeSREReport(t, "PG_SAGE_SRE_MATRIX_REPORT", b)
}

func TestLakebaseSRERealGatewayInvestigator(t *testing.T) {
	p, ctx, policy := sreNativePool(t)
	if os.Getenv("PG_SAGE_LIVE_LLM") != "1" || os.Getenv("SAGE_LLM_AUTH") != "databricks" || os.Getenv("SAGE_LLM_API_KEY") != "" {
		t.Fatal("real SRE acceptance requires explicit PG_SAGE_LIVE_LLM=1 and Databricks OAuth without an API key")
	}
	endpoint, model := os.Getenv("SAGE_LLM_ENDPOINT"), os.Getenv("SAGE_LLM_MODEL")
	if !strings.HasPrefix(endpoint, "https://") || model == "" {
		t.Fatal("explicit Gateway endpoint and model required")
	}
	modelClient := llm.New(&config.LLMConfig{Enabled: true, Auth: "databricks", Endpoint: endpoint, Model: model, TimeoutSeconds: 45}, func(string, string, ...any) {})
	limits := sre.DefaultLimits()
	limits.DatabaseDailyTokens, limits.DeploymentDailyTokens = 100000, 100000
	store, err := sre.NewPostgresStore(p, limits)
	if err != nil {
		t.Fatal(err)
	}
	cfg := sre.DefaultCoordinatorConfig("native-sre:" + string(sre.NewUUID()))
	cfg.AutomaticStart = false
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: store, Runner: probes.NewRunner(p, probes.Catalog(), nil, policy),
		ReadPolicy: policy, Config: cfg, Model: modelClient, Investigator: &sre.InvestigatorConfig{Explainer: sre.NewStatementExplainer(p)}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var actionsBefore int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&actionsBefore); err != nil {
		t.Fatal(err)
	}
	trigger := sre.Trigger{CaseID: "acceptance:" + string(sre.NewUUID()), Kind: sre.TriggerOperator,
		Subject:        "Synthetic acceptance: investigate only this database. Read database statistics using a tool before concluding and cite the newly collected evidence. No application incident is asserted.",
		IdempotencyKey: "acceptance:" + string(sre.NewUUID()), Actor: "native-acceptance"}
	inv, created, err := coord.Start(ctx, trigger)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	if err := coord.Investigate(ctx, inv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, scope, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := got.Summary.Investigator
	if !got.State.Terminal() || got.State == sre.StateFailed || run == nil {
		t.Fatalf("lifecycle state=%s failure=%s transcript=%v", got.State, got.FailureCode, run != nil)
	}
	t.Logf("case=%s state=%s model_calls=%d tool_calls=%d tokens=%d stop=%s", inv.ID, got.State, run.ModelCalls, run.ToolCalls, run.Tokens, run.Stop)
	if run.ModelCalls < 2 || run.Tokens <= 0 || run.Stop != "final" || got.Summary.ModelConclusion == nil {
		t.Fatalf("real tool-loop gate not met: %+v", run)
	}
	evidence, err := store.Evidence(ctx, scope, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[sre.UUID]sre.Evidence{}
	for _, e := range evidence {
		if !e.VerifyHash() {
			t.Fatal("bad evidence hash")
		}
		byID[e.ID] = e
	}
	var selected map[sre.UUID]bool = map[sre.UUID]bool{}
	for _, step := range run.Steps {
		if step.Tool != sre.ToolRunProbe && step.Tool != sre.ToolStatView && step.Tool != sre.ToolExplain {
			continue
		}
		if e, ok := byID[step.EvidenceID]; ok && step.Digest == hex.EncodeToString(e.SHA256) && policy.Allows(probes.ID(e.ProbeID)) {
			selected[e.ID] = true
		}
	}
	var cited bool
	if n := got.Summary.Narrative; n != nil {
		for _, claim := range n.Claims {
			for _, id := range claim.EvidenceIDs {
				if selected[id] {
					cited = true
				}
			}
		}
	}
	if !cited {
		t.Fatal("accepted final does not cite newly executed model-selected evidence")
	}
	if err := store.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatal(err)
	}
	again, created, err := coord.Start(ctx, trigger)
	if err != nil || created || again.ID != inv.ID {
		t.Fatal("idempotent lifecycle readback failed")
	}
	var after int
	_ = p.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&after)
	if after != actionsBefore {
		t.Fatal("read-only SRE case changed action ledger")
	}
	service := sre.NewService(policy.Database, coord, store)
	view, err := service.Transcript(ctx, inv.ID, sre.TranscriptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(view)
	writeSREReport(t, "PG_SAGE_SRE_TRANSCRIPT_REPORT", b)
}
