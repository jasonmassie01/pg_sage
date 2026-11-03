package lakebaselive

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/store"
)

// The fixture supplies an explicit operator correlation hypothesis; the actual
// bounded table probe and approval/execution/rollback use production services.
// This proves structural lifecycle compatibility, not automatic diagnosis,
// workload improvement, or incident recovery.
func TestLakebaseSREStatisticsMaintenance(t *testing.T) {
	p, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(string(sre.NewUUID()), "-", "")[:12]
	table := "maintenance_" + suffix
	exec(t, p, ctx, "CREATE SCHEMA IF NOT EXISTS sage_live")
	exec(t, p, ctx, "CREATE TABLE sage_live."+table+"(a int,b int)")
	exec(t, p, ctx, "INSERT INTO sage_live."+table+" SELECT g%10,g%10 FROM generate_series(1,1000)g")
	exec(t, p, ctx, "ANALYZE sage_live."+table)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := p.Exec(cleanup, "DROP TABLE IF EXISTS sage_live."+table+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	st, err := sre.NewPostgresStore(p, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := &probes.ReadPolicy{Version: 1, Scope: "database", Database: "pg_sage_test", Binding: probes.PoolBinding(p), Allowed: []probes.ID{probes.StatTables}}
	runner := probes.NewRunner(p, probes.Catalog(), nil, policy)
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: runner, ReadPolicy: policy, Config: sre.DefaultCoordinatorConfig("native-maintenance:" + suffix)})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "maintenance:" + suffix, Kind: sre.TriggerOperator, Subject: "sage_live." + table})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.Claim(ctx, scope, inv.ID, sre.NewUUID())
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(ctx, probes.StatTables, probes.Args{})
	if result.Status != probes.StatusOK {
		t.Fatalf("bounded actual table probe: %+v", result)
	}
	if _, err = st.CommitStep(ctx, lease, sre.StepResult{IdempotencyKey: "operator-table-review", Results: []probes.Result{result}, NextState: sre.StateEvaluating}); err != nil {
		t.Fatal(err)
	}
	evidence, err := st.Evidence(ctx, scope, inv.ID)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("stored evidence: %d %v", len(evidence), err)
	}
	inv, err = st.Conclude(ctx, lease, sre.Conclusion{State: sre.StateConcluded, Summary: sre.Summary{Family: "stat_views", Root: "operator_table_review", Conclusive: true}, Hypotheses: []sre.HypothesisRecord{{GraphVersion: "maintenance-live-v1", Family: "stat_views", Node: "operator_table_review", Label: "Operator reviewed observed table counters", RefutationProbe: "stat_tables", Status: sre.HypothesisRoot, Support: []sre.Fact{{EvidenceID: evidence[0].ID, Text: "Table counters were observed; column correlation is an explicit operator hypothesis"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = "lakebase"
	cfg.Trust.Level = "advisory"
	cfg.Safety.QueryTimeoutMs = 10000
	e := executor.New(p, cfg, time.Time{}, func(level, format string, args ...any) { t.Logf("%s %s", level, fmt.Sprintf(format, args...)) })
	e.WithDatabaseName("pg_sage_test")
	if err = e.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	m, err := action.NewMaintenanceService(action.MaintenanceDeps{Service: sre.NewService("pg_sage_test", coord, st), Pool: p, Executor: e})
	if err != nil {
		t.Fatal(err)
	}
	e.SetApprovedActionRunner(m)
	request := action.StatisticsRequest{Schema: "sage_live", Table: table, Columns: []string{"a", "b"}, Kinds: []string{"dependencies", "ndistinct"}, EvidenceID: evidence[0].ID, EvidenceSHA256: hex.EncodeToString(evidence[0].SHA256), Rationale: "Operator hypothesis: owned fixture columns a and b are correlated"}
	proposal, err := m.ProposeStatistics(ctx, inv.ID, request, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	if err = proposal.Contract.Validate(); err != nil {
		t.Fatal(err)
	}
	again, err := m.ProposeStatistics(ctx, inv.ID, request, "user:1")
	if err != nil || again.ID != proposal.ID {
		t.Fatalf("proposal replay: %+v %v", again, err)
	}
	proposal, err = m.RequestExecution(ctx, proposal.ID, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	queue := store.NewActionStore(p)
	approved, err := queue.Approve(ctx, proposal.QueueID, 1)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := e.RunApprovedAction(ctx, *approved, 1)
	if err != nil {
		t.Fatal(err)
	}
	if applied.ActionLogID <= 0 || applied.VerificationStatus != "verified" {
		t.Fatalf("applied: %+v", applied)
	}
	proposal, err = m.Get(ctx, proposal.ID)
	if err != nil || proposal.State != "executed" || proposal.StatisticsOID == 0 {
		t.Fatalf("proposal: %+v %v", proposal, err)
	}
	if _, err = e.RunApprovedAction(ctx, *approved, 1); err == nil {
		t.Fatal("replayed approval executed")
	}
	if _, err = e.ExecuteManual(ctx, proposal.FindingID, proposal.SQL, proposal.RollbackSQL, nil); err == nil {
		t.Fatal("manual bypass accepted")
	}
	var decisionID int64
	var verdict string
	if err = p.QueryRow(ctx, `SELECT a.decision_id,d.verdict FROM sage.action_log a JOIN sage.decision d ON d.id=a.decision_id WHERE a.id=$1 AND a.sql_executed=$2`, applied.ActionLogID, proposal.SQL).Scan(&decisionID, &verdict); err != nil || decisionID <= 0 || verdict != "execute" {
		t.Fatalf("durable policy decision=%d verdict=%s error=%v", decisionID, verdict, err)
	}
	// This binding is unique to this one fixture. Exercise only its scoped
	// production retention path; never run the database-wide generic cleaner.
	ownedProposalID := proposal.ID
	var scopeCases int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM sage.sre_investigations
		WHERE deployment_id=$1 AND database_id=$2`, string(scope.DeploymentID),
		string(scope.DatabaseID)).Scan(&scopeCases); err != nil || scopeCases != 1 {
		t.Fatalf("native maintenance retention scope is not exclusively owned: count=%d err=%v", scopeCases, err)
	}
	retentionPolicy := sre.RetentionPolicy{EvidenceAge: 30 * 24 * time.Hour,
		TimelineAge: 90 * 24 * time.Hour, BatchSize: 50}
	ageOwnInvestigation := func() {
		t.Helper()
		if _, err := p.Exec(ctx, `UPDATE sage.sre_investigations
			SET created_at=now()-interval '401 days', updated_at=now()-interval '400 days'
			WHERE deployment_id=$1 AND database_id=$2 AND id=$3`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(inv.ID)); err != nil {
			t.Fatal(err)
		}
	}
	ageOwnInvestigation()
	if purged, err := st.Purge(ctx, scope, retentionPolicy); err != nil || purged != (sre.PurgeResult{}) {
		t.Fatalf("native recent-maintenance retention hold: %+v %v", purged, err)
	}
	// Inject an explicitly labeled post-execution uncertainty in this owned
	// ledger. This tests retention, not an actual provider crash or recovery.
	if err := st.WithInvestigationLock(ctx, scope, inv.ID, "native-retention-fixture", func(tx pgx.Tx, emit sre.Emit) error {
		if _, err := tx.Exec(ctx, `UPDATE sage.sre_maintenance_proposals
			SET state='uncertain', proposal=jsonb_set(proposal,'{state}','"uncertain"'),
			    created_at=now()-interval '401 days', updated_at=now()-interval '400 days'
			WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3 AND id=$4`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(inv.ID), string(ownedProposalID)); err != nil {
			return err
		}
		return emit("action_failed", map[string]any{"proposal_id": proposal.ID,
			"state": "uncertain", "fixture_injected": true,
			"detail": "controlled retention fixture; no actual provider crash"})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A failed retention assertion must not leave an indefinitely held,
		// injected uncertainty. Only this unique fixture ledger is removed.
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := p.Exec(cleanup, `DELETE FROM sage.sre_maintenance_proposals
			WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3 AND id=$4`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(inv.ID), string(ownedProposalID)); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			if _, err := st.Purge(cleanup, scope, retentionPolicy); err != nil {
				t.Errorf("cleanup owned native retention fixture: %v", err)
			}
		}
	})
	if purged, err := st.Purge(ctx, scope, retentionPolicy); err != nil || purged != (sre.PurgeResult{}) {
		t.Fatalf("native injected-unresolved retention hold: %+v %v", purged, err)
	}
	proposal, err = m.Rollback(ctx, proposal.ID, "native owned fixture verification complete", 1)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.State != "rolled_back" {
		t.Fatalf("rollback: %+v", proposal)
	}
	var remains bool
	if err = p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_statistic_ext WHERE oid=$1)`, proposal.StatisticsOID).Scan(&remains); err != nil || remains {
		t.Fatalf("exact object removed=%v error=%v", !remains, err)
	}
	if err = st.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatal(err)
	}
	events, err := st.Events(ctx, scope, inv.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("native terminal timeline: %v", err)
	}
	head := hex.EncodeToString(events[len(events)-1].Hash)
	if _, err := p.Exec(ctx, `UPDATE sage.sre_maintenance_proposals
		SET created_at=now()-interval '401 days', updated_at=now()-interval '400 days'
		WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3 AND id=$4`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(inv.ID), string(ownedProposalID)); err != nil {
		t.Fatal(err)
	}
	if purged, err := st.Purge(ctx, scope, retentionPolicy); err != nil || purged.InvestigationsPurged != 1 {
		t.Fatalf("native resolved-owned timeline cascade: %+v %v", purged, err)
	}
	if _, err := m.Get(ctx, proposal.ID); !errors.Is(err, action.ErrProposalNotFound) {
		t.Fatalf("native owned proposal did not cascade: %v", err)
	}
	stones, err := st.Tombstones(ctx, scope, inv.ID)
	if err != nil || len(stones) != 1 || stones[0].Kind != "investigation" || stones[0].Reason != "retention" {
		t.Fatalf("native retention tombstone: %+v %v", stones, err)
	}
	var detail struct {
		Head string `json:"head_hash"`
	}
	if err := json.Unmarshal(stones[0].Detail, &detail); err != nil || detail.Head != head {
		t.Fatalf("native tombstone chain head: %+v %v", detail, err)
	}
	t.Logf("native scoped retention verified: recent and injected-uncertain holds, actual typed rollback, terminal owned cascade and tombstone head=%s; no generic cleaner or actual-crash recovery claim", head)
	t.Logf("native typed maintenance proposal=%s investigation=%s evidence=%s queue=%d action=%d decision=%d statistics_oid=%d; object built then removed; hash chain verified; ANALYZE statistics not restored; workload benefit/incident recovery unproven", proposal.ID, inv.ID, evidence[0].ID, proposal.QueueID, applied.ActionLogID, decisionID, proposal.StatisticsOID)
}
