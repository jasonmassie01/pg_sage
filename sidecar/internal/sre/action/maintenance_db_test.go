package action

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/store"
)

type maintenanceHarness struct {
	*actionHarness
	m     *MaintenanceService
	exec  *executor.Executor
	cfg   *config.Config
	req   StatisticsRequest
	table string
}

func newMaintenanceHarness(t *testing.T, mutate ...func(*probes.Result)) *maintenanceHarness {
	t.Helper()
	h := newActionHarness(t, nil)
	table := "maintenance_" + strings.ReplaceAll(string(sre.NewUUID()), "-", "")[:12]
	for _, sql := range []string{"CREATE TABLE public." + table + "(a int,b int)", "INSERT INTO public." + table + " SELECT g%10,g%10 FROM generate_series(1,1000)g", "ANALYZE public." + table} {
		if _, err := h.pool.Exec(h.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { h.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table+" CASCADE") })
	policy := &probes.ReadPolicy{Version: 1, Scope: "database", Database: h.pool.Config().ConnConfig.Database, Binding: probes.PoolBinding(h.pool), Allowed: []probes.ID{probes.StatTables}}
	runner := probes.NewRunner(h.pool, probes.Catalog(), nil, policy)
	c, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: h.st, Runner: runner, ReadPolicy: policy, Config: sre.DefaultCoordinatorConfig("maintenance:" + string(sre.NewUUID()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Bind(h.ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := c.Start(h.ctx, sre.Trigger{CaseID: "maintenance:" + table, Kind: sre.TriggerOperator, Subject: table})
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := c.Scope()
	lease, err := h.st.Claim(h.ctx, scope, inv.ID, sre.NewUUID())
	if err != nil {
		t.Fatal(err)
	}
	res := runner.Run(h.ctx, probes.StatTables, probes.Args{})
	if res.Status != probes.StatusOK {
		t.Fatalf("live table probe: %+v", res)
	}
	for _, fn := range mutate {
		fn(&res)
	}
	if _, err = h.st.CommitStep(h.ctx, lease, sre.StepResult{IdempotencyKey: "maintenance-table-observation", Results: []probes.Result{res}, NextState: sre.StateEvaluating}); err != nil {
		t.Fatal(err)
	}
	ev, err := h.st.Evidence(h.ctx, scope, inv.ID)
	if err != nil || len(ev) != 1 {
		t.Fatalf("evidence: %v %v", ev, err)
	}
	inv, err = h.st.Conclude(h.ctx, lease, sre.Conclusion{State: sre.StateConcluded, Summary: sre.Summary{Family: "stat_views", Root: "table_observed", Conclusive: true}, Hypotheses: []sre.HypothesisRecord{{GraphVersion: "maintenance-test-v1", Family: "stat_views", Node: "table_observed", Label: "Table observed for operator statistics experiment", RefutationProbe: "stat_tables", Status: sre.HypothesisRoot, Support: []sre.Fact{{EvidenceID: ev[0].ID, Text: "table counters observed; correlation remains an operator hypothesis"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	h.inv, h.coord, h.svc = inv, c, sre.NewService(h.pool.Config().ConnConfig.Database, c, h.st)
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = "lakebase"
	cfg.Trust.Level = "advisory"
	cfg.Safety.QueryTimeoutMs = 10000
	e := executor.New(h.pool, cfg, time.Time{}, func(string, string, ...any) {})
	e.WithDatabaseName(h.pool.Config().ConnConfig.Database)
	if err = e.EnableStandingPolicy(h.ctx, "unattended", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	m, err := NewMaintenanceService(MaintenanceDeps{Service: h.svc, Pool: h.pool, Executor: e})
	if err != nil {
		t.Fatal(err)
	}
	e.SetApprovedActionRunner(m)
	req := StatisticsRequest{Schema: "public", Table: table, Columns: []string{"a", "b"}, Kinds: []string{"dependencies", "ndistinct"}, EvidenceID: ev[0].ID, EvidenceSHA256: hex.EncodeToString(ev[0].SHA256), Rationale: "Operator hypothesis: fixture columns a and b are correlated"}
	return &maintenanceHarness{actionHarness: h, m: m, exec: e, cfg: cfg, req: req, table: table}
}
func (h *maintenanceHarness) propose(t *testing.T) MaintenanceProposal {
	t.Helper()
	p, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (h *maintenanceHarness) request(t *testing.T) MaintenanceProposal {
	t.Helper()
	p := h.propose(t)
	p, err := h.m.RequestExecution(h.ctx, p.ID, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func (h *maintenanceHarness) approve(t *testing.T, p MaintenanceProposal) store.QueuedAction {
	t.Helper()
	a, err := h.as.Approve(h.ctx, p.QueueID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return *a
}
func (h *maintenanceHarness) exists(t *testing.T, p MaintenanceProposal) bool {
	t.Helper()
	var b bool
	err := h.pool.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM pg_statistic_ext WHERE stxname=$1)`, "sage_stx_"+strings.ReplaceAll(string(p.ID), "-", "")).Scan(&b)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMaintenanceProposalApprovalExecutionVerificationRollback(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.propose(t)
	if err := p.Contract.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Contract.RequiredPermissions, ";") != "CREATE privilege on the statistics schema;ownership of the target table" {
		t.Fatalf("unexpected permission contract: %v", p.Contract.RequiredPermissions)
	}
	if !strings.Contains(p.Contract.ResidualRisk, "cannot restore prior ANALYZE") {
		t.Fatal("rollback residual missing")
	}
	again := h.propose(t)
	if again.ID != p.ID {
		t.Fatal("proposal was not idempotent")
	}
	if h.exists(t, p) {
		t.Fatal("proposal executed SQL")
	}
	p, err := h.m.RequestExecution(h.ctx, p.ID, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	again, err = h.m.RequestExecution(h.ctx, p.ID, "user:1")
	if err != nil || again.QueueID != p.QueueID {
		t.Fatalf("request replay: %+v %v", again, err)
	}
	if h.exists(t, p) {
		t.Fatal("pending approval executed SQL")
	}
	if _, err = h.exec.ExecuteManual(h.ctx, p.FindingID, p.SQL, p.RollbackSQL, nil); err == nil {
		t.Fatal("manual bypass allowed")
	}
	a := h.approve(t, p)
	result, err := h.exec.RunApprovedAction(h.ctx, a, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.ActionLogID <= 0 || result.VerificationStatus != "verified" || !h.exists(t, p) {
		t.Fatalf("execution result: %+v", result)
	}
	p, err = h.m.Get(h.ctx, p.ID)
	if err != nil || p.StatisticsOID == 0 || p.State != "executed" {
		t.Fatalf("proposal: %+v %v", p, err)
	}
	if _, err = h.exec.RunApprovedAction(h.ctx, a, 1); err == nil {
		t.Fatal("approval replay executed")
	}
	if err = h.exec.RollbackAction(h.ctx, p.ActionLogID, "bypass"); err == nil {
		t.Fatal("generic rollback bypass allowed")
	}
	p, err = h.m.Rollback(h.ctx, p.ID, "fixture cleanup", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "rolled_back" || h.exists(t, p) {
		t.Fatalf("rollback state: %+v", p)
	}
	if _, err = h.m.Rollback(h.ctx, p.ID, "replay", 1); err == nil {
		t.Fatal("rollback replay allowed")
	}
	if err = h.st.VerifyEvents(h.ctx, h.inv.Scope, h.inv.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("proposal=%s queue=%d action=%d structural verification passed; exact object rollback passed; ANALYZE not restorable", p.ID, p.QueueID, p.ActionLogID)
}
func TestMaintenanceEvidenceAndApprovalRefusals(t *testing.T) {
	t.Run("wrong evidence digest", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		h.req.EvidenceSHA256 = strings.Repeat("0", 64)
		if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("wrong digest accepted")
		}
	})
	t.Run("foreign evidence", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		h.req.EvidenceID = sre.NewUUID()
		if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("foreign evidence accepted")
		}
	})
	t.Run("unobserved table", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		other := h.table + "x"
		_, err := h.pool.Exec(h.ctx, "CREATE TABLE public."+other+"(a int,b int)")
		if err != nil {
			t.Fatal(err)
		}
		defer h.pool.Exec(h.ctx, "DROP TABLE public."+other)
		h.req.Table = other
		if _, err = h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("unobserved table accepted")
		}
	})
	t.Run("legacy name-only evidence", func(t *testing.T) {
		h := newMaintenanceHarness(t, func(res *probes.Result) {
			for _, row := range res.Rows {
				delete(row, "database_oid")
				delete(row, "table_oid")
			}
		})
		if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("legacy name-only evidence accepted")
		}
	})
	t.Run("recreated before proposal", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		if _, err := h.pool.Exec(h.ctx, "DROP TABLE public."+h.table+"; CREATE TABLE public."+h.table+"(a int,b int)"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("recreated table was accepted using prior physical table evidence")
		}
	})
	t.Run("stale evidence", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		h.m.MaxEvidenceAge = time.Nanosecond
		if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("stale evidence accepted")
		}
	})
	t.Run("purged evidence", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		_, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_investigations SET evidence_purged_at=clock_timestamp() WHERE id=$1`, string(h.inv.ID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("purged evidence accepted")
		}
	})
	t.Run("wrong pool policy", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		current := h.coord.ReadPolicy()
		current.Binding = "wrong"
		coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: h.st, Runner: probes.NewRunner(h.pool, probes.Catalog(), nil, current), ReadPolicy: current, Config: sre.DefaultCoordinatorConfig("foreign:" + string(sre.NewUUID()))})
		if err != nil {
			t.Fatal(err)
		}

		h.m.Service = sre.NewService(h.svc.Name(), coord, h.st)
		if _, err = h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
			t.Fatal("foreign coordinator accepted")
		}
	})
	for _, kind := range []string{"not approved", "wrong approver", "expired approval", "runner missing", "changed table", "changed column", "emergency", "observation", "policy removed"} {
		t.Run(kind, func(t *testing.T) {
			h := newMaintenanceHarness(t)
			p := h.request(t)
			a := store.QueuedAction{ID: p.QueueID, FindingID: p.FindingID, ProposedSQL: p.SQL, RollbackSQL: p.RollbackSQL, ActionType: "create_statistics", IdentityKey: MaintenanceIdentityPrefix + string(p.ID)}
			user := 1
			if kind != "not approved" {
				a = h.approve(t, p)
			}
			switch kind {
			case "wrong approver":
				user = 2
			case "expired approval":
				_, err := h.pool.Exec(h.ctx, "UPDATE sage.action_queue SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", p.QueueID)
				if err != nil {
					t.Fatal(err)
				}
			case "runner missing":
				h.exec.SetApprovedActionRunner(nil)
			case "changed table":
				_, err := h.pool.Exec(h.ctx, "DROP TABLE public."+h.table+"; CREATE TABLE public."+h.table+"(a int,b int)")
				if err != nil {
					t.Fatal(err)
				}
			case "changed column":
				_, err := h.pool.Exec(h.ctx, "ALTER TABLE public."+h.table+" ALTER COLUMN b TYPE bigint")
				if err != nil {
					t.Fatal(err)
				}
			case "emergency":
				h.exec.WithEmergencyStopCheck(func(context.Context) bool { return true })
			case "observation":
				h.cfg.Trust.Level = "observation"
			case "policy removed":
				h.exec.EnableStandingPolicyDocument(policy.Document{}, nil)
			}
			if _, err := h.exec.RunApprovedAction(h.ctx, a, user); err == nil {
				t.Fatalf("%s was allowed", kind)
			}
			if h.exists(t, p) {
				t.Fatalf("%s created statistics", kind)
			}
		})
	}
}
func TestMaintenanceRollbackRefusesReplacementObject(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.request(t)
	a := h.approve(t, p)
	if _, err := h.exec.RunApprovedAction(h.ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	p, err := h.m.Get(h.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{p.RollbackSQL, p.SQL} {
		if _, err = h.pool.Exec(h.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.m.Rollback(h.ctx, p.ID, "must preserve replacement", 1); err == nil {
		t.Fatal("replacement object was dropped")
	}
	if !h.exists(t, p) {
		t.Fatal("replacement object missing")
	}
	t.Log(fmt.Sprintf("replacement statistics survived guarded rollback for proposal %s", p.ID))
}
