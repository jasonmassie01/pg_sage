package action

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/sre"
)

var maintenanceRetentionPolicy = sre.RetentionPolicy{EvidenceAge: 30 * 24 * time.Hour,
	TimelineAge: 90 * 24 * time.Hour, BatchSize: 50}

func executeRetentionMaintenance(t *testing.T, h *maintenanceHarness) MaintenanceProposal {
	t.Helper()
	p := h.request(t)
	run, err := h.exec.RunApprovedAction(h.ctx, h.approve(t, p), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Complete the same durable queue outcome as the API/orchestrator.
	if err := h.as.MarkExecuted(h.ctx, p.QueueID, run.ActionLogID, run.VerificationStatus); err != nil {
		t.Fatal(err)
	}
	p, err = h.m.Get(h.ctx, p.ID)
	if err != nil || p.State != "executed" || p.ActionLogID <= 0 || p.StatisticsOID == 0 {
		t.Fatalf("executed maintenance: %+v %v", p, err)
	}
	return p
}

func ageMaintenanceForRetention(t *testing.T, h *maintenanceHarness, p MaintenanceProposal, days int) {
	t.Helper()
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_investigations
		SET created_at=now()-interval '401 days', updated_at=now()-interval '400 days'
		WHERE deployment_id=$1 AND database_id=$2 AND id=$3`,
		string(h.inv.Scope.DeploymentID), string(h.inv.Scope.DatabaseID), string(h.inv.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_maintenance_proposals
		SET created_at=now()-interval '401 days', updated_at=now()-make_interval(days => $2)
		WHERE id=$1`, string(p.ID), days); err != nil {
		t.Fatal(err)
	}
}

type maintenanceAuditIDs struct{ decision, verification int64 }

func ageMaintenanceAudit(t *testing.T, h *maintenanceHarness, p MaintenanceProposal) maintenanceAuditIDs {
	t.Helper()
	var ids maintenanceAuditIDs
	var credit float64
	if err := h.pool.QueryRow(h.ctx, `SELECT COALESCE(decision_id,0),
		COALESCE(verification_id,0), COALESCE(toil_minutes_saved,0)
		FROM sage.action_log WHERE id=$1`, p.ActionLogID).Scan(&ids.decision, &ids.verification, &credit); err != nil {
		t.Fatal(err)
	}
	if ids.decision == 0 || ids.verification == 0 || credit != 0 {
		t.Fatalf("missing audit links or unexpected maintenance credit: %+v credit=%v", ids, credit)
	}
	for _, q := range []struct {
		sql string
		id  any
	}{
		{`UPDATE sage.action_log SET executed_at=now()-interval '400 days' WHERE id=$1`, p.ActionLogID},
		{`UPDATE sage.action_queue SET proposed_at=now()-interval '400 days',
			decided_at=now()-interval '400 days', expires_at=now()-interval '399 days' WHERE id=$1`, p.QueueID},
		{`UPDATE sage.findings SET status='resolved', resolved_at=now()-interval '400 days' WHERE id=$1`, p.FindingID},
		{`UPDATE sage.verification SET created_at=now()-interval '400 days',
			updated_at=now()-interval '400 days', completed_at=now()-interval '400 days',
			verdict='unverifiable' WHERE id=$1`, ids.verification},
		{`UPDATE sage.decision SET created_at=now()-interval '400 days',
			last_seen_at=now()-interval '400 days', deadline_hard_at=now()-interval '399 days',
			resolved_at=now()-interval '399 days' WHERE id=$1`, ids.decision},
	} {
		if _, err := h.pool.Exec(h.ctx, q.sql, q.id); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func runMaintenanceCleaner(t *testing.T, h *maintenanceHarness) {
	t.Helper()
	cfg := &config.Config{Retention: config.RetentionConfig{ActionsDays: 1,
		FindingsDays: 1, DecisionsDays: 1}}
	var failures []string
	retention.New(h.pool, cfg, func(level, format string, args ...any) {
		if level == "ERROR" {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}).WithPacing(time.Nanosecond, 30*time.Second).RunOnce(h.ctx)
	if len(failures) != 0 {
		t.Fatalf("retention errors: %v", failures)
	}
}

func assertMaintenanceAudit(t *testing.T, h *maintenanceHarness, p MaintenanceProposal,
	ids maintenanceAuditIDs, kept bool) {
	t.Helper()
	for _, q := range []struct {
		table string
		id    any
	}{{"findings", p.FindingID}, {"action_queue", p.QueueID}, {"action_log", p.ActionLogID},
		{"verification", ids.verification}, {"decision", ids.decision}} {
		var exists bool
		if err := h.pool.QueryRow(h.ctx, "SELECT EXISTS(SELECT 1 FROM sage."+q.table+" WHERE id=$1)", q.id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != kept {
			t.Errorf("%s dependency %v kept=%v, want %v", q.table, q.id, exists, kept)
		}
	}
}

// Retaining a proposal must preserve a usable typed inverse, including when
// the evidence window and generic actions window have expired. Finalized work
// still cascades with its timeline and then releases every ordinary dependency.
func TestMaintenanceRetentionPreservesRollbackThenReleasesAudit(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := executeRetentionMaintenance(t, h)
	ids := ageMaintenanceAudit(t, h, p)
	ageMaintenanceForRetention(t, h, p, 31)
	res, err := h.st.Purge(h.ctx, h.inv.Scope, maintenanceRetentionPolicy)
	if err != nil || res.EvidencePurged != 1 || res.InvestigationsPurged != 0 {
		t.Fatalf("evidence/timeline windows: %+v %v", res, err)
	}
	inv, err := h.st.Get(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil || inv.EvidencePurgedAt.IsZero() {
		t.Fatalf("evidence was not tombstoned: %+v %v", inv, err)
	}
	stones, err := h.st.Tombstones(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil || len(stones) != 1 || stones[0].Kind != "evidence" || stones[0].RowCount != 1 {
		t.Fatalf("evidence tombstone: %+v %v", stones, err)
	}
	runMaintenanceCleaner(t, h)
	assertMaintenanceAudit(t, h, p, ids, true)
	p, err = h.m.Rollback(h.ctx, p.ID, "operator rollback after evidence retention", 1)
	if err != nil || p.State != "rolled_back" || h.exists(t, p) {
		t.Fatalf("typed inverse after retention: %+v %v", p, err)
	}
	if err := h.st.VerifyEvents(h.ctx, h.inv.Scope, h.inv.ID); err != nil {
		t.Fatal(err)
	}
	events, err := h.st.Events(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("events: %v", err)
	}
	head := hex.EncodeToString(events[len(events)-1].Hash)
	ageMaintenanceForRetention(t, h, p, 91)
	res, err = h.st.Purge(h.ctx, h.inv.Scope, maintenanceRetentionPolicy)
	if err != nil || res.InvestigationsPurged != 1 {
		t.Fatalf("terminal maintenance did not cascade: %+v %v", res, err)
	}
	if _, err := h.m.Get(h.ctx, p.ID); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("cascaded proposal still readable: %v", err)
	}
	stones, err = h.st.Tombstones(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil || len(stones) != 2 {
		t.Fatalf("cascade tombstones: %+v %v", stones, err)
	}
	var retainedHead string
	for _, stone := range stones {
		if stone.Kind == "investigation" {
			var detail struct {
				Head string `json:"head_hash"`
			}
			if err := json.Unmarshal(stone.Detail, &detail); err != nil {
				t.Fatal(err)
			}
			retainedHead = detail.Head
		}
	}
	if retainedHead != head {
		t.Fatalf("tombstone lost terminal chain head: %q, want %q", retainedHead, head)
	}
	runMaintenanceCleaner(t, h)
	assertMaintenanceAudit(t, h, p, ids, false)
}

// The executor can commit before the bridge records ActionLogID. Preserve
// that exact unresolved action and audit, without holding unrelated actions
// that merely share its finding or use a different inverse. This proves
// retention of the crash record, not automatic crash recovery or rollback.
func TestMaintenanceRetentionKeepsPostCommitPreSaveCrashRecord(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := executeRetentionMaintenance(t, h)
	ids := ageMaintenanceAudit(t, h, p)
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_maintenance_proposals
		SET state='executing', proposal=jsonb_set(jsonb_set(jsonb_set(proposal,
		    '{state}', '"executing"'), '{action_log_id}', '0'), '{statistics_oid}', '0')
		WHERE id=$1`, string(p.ID)); err != nil {
		t.Fatal(err)
	}
	ageMaintenanceForRetention(t, h, p, 400)
	var unrelated []int64
	for _, sql := range []struct{ forward, inverse string }{
		{"SELECT 1", p.RollbackSQL}, {p.SQL, "SELECT 1"},
	} {
		var id int64
		if err := h.pool.QueryRow(h.ctx, `INSERT INTO sage.action_log
			(finding_id, action_type, sql_executed, rollback_sql, outcome, executed_at)
			VALUES ($1, 'create_statistics', $2, $3, 'failed', now()-interval '400 days')
			RETURNING id`, p.FindingID, sql.forward, sql.inverse).Scan(&id); err != nil {
			t.Fatal(err)
		}
		unrelated = append(unrelated, id)
	}
	res, err := h.st.Purge(h.ctx, h.inv.Scope, maintenanceRetentionPolicy)
	if err != nil || res != (sre.PurgeResult{}) {
		t.Fatalf("unresolved crash record was purged: %+v %v", res, err)
	}
	runMaintenanceCleaner(t, h)
	assertMaintenanceAudit(t, h, p, ids, true)
	for _, id := range unrelated {
		var exists bool
		if err := h.pool.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM sage.action_log WHERE id=$1)`, id).Scan(&exists); err != nil || exists {
			t.Fatalf("unrelated same-finding action %d held: %v %v", id, exists, err)
		}
	}
	// Once the exact action is recorded, a different ID with identical SQL
	// cannot inherit its retention lifetime.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_maintenance_proposals
		SET state='executed', proposal=$2 WHERE id=$1`, string(p.ID), raw); err != nil {
		t.Fatal(err)
	}
	var replacement int64
	if err := h.pool.QueryRow(h.ctx, `INSERT INTO sage.action_log
		(finding_id, action_type, sql_executed, rollback_sql, outcome, executed_at)
		VALUES ($1, 'create_statistics', $2, $3, 'failed', now()-interval '400 days')
		RETURNING id`, p.FindingID, p.SQL, p.RollbackSQL).Scan(&replacement); err != nil {
		t.Fatal(err)
	}
	runMaintenanceCleaner(t, h)
	assertMaintenanceAudit(t, h, p, ids, true)
	var exists bool
	if err := h.pool.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM sage.action_log WHERE id=$1)`, replacement).Scan(&exists); err != nil || exists {
		t.Fatalf("replacement action inherited exact-ID hold: %v %v", exists, err)
	}
}

// Execute and rollback release their coordination transaction before SQL runs.
// Retention must honor the durable in-flight state throughout that interval.
func TestMaintenanceRetentionRacesBlockedExecutionAndRollback(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.request(t)
	a := h.approve(t, p)
	for _, state := range []string{"executing", "rolling_back"} {
		lock, err := h.pool.Begin(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(context.Background())
		if _, err := lock.Exec(h.ctx, "LOCK TABLE public."+h.table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			lock.Rollback(context.Background())
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			if state == "executing" {
				_, err := h.exec.RunApprovedAction(h.ctx, a, 1)
				finished <- err
			} else {
				_, err := h.m.Rollback(h.ctx, p.ID, "explicit race-test rollback", 1)
				finished <- err
			}
		}()
		wait := time.NewTimer(5 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		claimed := false
		for !claimed {
			select {
			case err := <-finished:
				lock.Rollback(context.Background())
				t.Fatalf("%s ended before table-lock test: %v", state, err)
			case <-wait.C:
				lock.Rollback(context.Background())
				t.Fatalf("%s was not durably claimed", state)
			case <-tick.C:
				current, err := h.m.Get(h.ctx, p.ID)
				if err != nil {
					lock.Rollback(context.Background())
					t.Fatal(err)
				}
				claimed = current.State == state
			}
		}
		tick.Stop()
		wait.Stop()
		ageMaintenanceForRetention(t, h, p, 400)
		res, purgeErr := h.st.Purge(h.ctx, h.inv.Scope, maintenanceRetentionPolicy)
		if err := lock.Rollback(h.ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-finished; err != nil {
			t.Fatalf("%s after racing retention: %v", state, err)
		}
		if purgeErr != nil || res != (sre.PurgeResult{}) {
			t.Fatalf("%s lost ledger/evidence while SQL blocked: %+v %v", state, res, purgeErr)
		}
		p, err = h.m.Get(h.ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if p.State != "rolled_back" || h.exists(t, p) {
		t.Fatalf("race lifecycle did not complete exact rollback: %+v", p)
	}
	if err := h.st.VerifyEvents(h.ctx, h.inv.Scope, h.inv.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceConstructorRequiresColocatedBinding(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*pgxpool.Config)
		ok   bool
	}{
		{"separate pools same target", func(*pgxpool.Config) {}, true},
		{"different user same target", func(c *pgxpool.Config) { c.ConnConfig.User = "another_role" }, true},
		{"different database", func(c *pgxpool.Config) { c.ConnConfig.Database = "control" }, false},
		{"different host", func(c *pgxpool.Config) { c.ConnConfig.Host = "127.0.0.2" }, false},
		{"different port", func(c *pgxpool.Config) { c.ConnConfig.Port = 2 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/target?sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			controlCfg := cfg.Copy()
			tc.edit(controlCfg)
			control, err := pgxpool.NewWithConfig(ctx, controlCfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(control.Close)
			target, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(target.Close)
			st, err := sre.NewPostgresStore(control, sre.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
				Runner: newScriptedRunner(), Config: sre.DefaultCoordinatorConfig("constructor")})
			if err != nil {
				t.Fatal(err)
			}
			exec := executor.New(target, config.DefaultConfig(), time.Time{}, func(string, string, ...any) {})
			m, err := NewMaintenanceService(MaintenanceDeps{Service: sre.NewService("target", coord, st), Pool: target, Executor: exec})
			if tc.ok {
				if err != nil || m == nil {
					t.Fatalf("colocated binding refused: %v", err)
				}
			} else if !errors.Is(err, sre.ErrInvalidRequest) || m != nil ||
				!strings.Contains(err.Error(), "colocated coordination and target") {
				t.Fatalf("split binding accepted or wrong error: %v", err)
			}
		})
	}
}

// Enqueue commits before the proposal update. The immutable UUID identity
// preserves exactly that queue item even if QueueID has not yet been saved.
func TestMaintenanceRetentionKeepsExactEnqueueIdentityBeforeProposalSave(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.request(t)
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_maintenance_proposals
		SET state='proposed', proposal=jsonb_set(jsonb_set(proposal,
		    '{state}', '"proposed"'), '{queue_id}', '0') WHERE id=$1`, string(p.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.action_queue SET status='expired',
		proposed_at=now()-interval '400 days', expires_at=now()-interval '399 days'
		WHERE id=$1`, p.QueueID); err != nil {
		t.Fatal(err)
	}
	var unrelated int
	if err := h.pool.QueryRow(h.ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, identity_key, proposed_at, expires_at)
		VALUES ('SELECT 1', 'safe', 'expired', $1, now()-interval '400 days',
		        now()-interval '399 days') RETURNING id`,
		MaintenanceIdentityPrefix+string(sre.NewUUID())).Scan(&unrelated); err != nil {
		t.Fatal(err)
	}
	runMaintenanceCleaner(t, h)
	for _, tc := range []struct {
		id   int
		kept bool
	}{{p.QueueID, true}, {unrelated, false}} {
		var exists bool
		if err := h.pool.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM sage.action_queue WHERE id=$1)`, tc.id).Scan(&exists); err != nil || exists != tc.kept {
			t.Fatalf("queue identity id=%d kept=%v want=%v err=%v", tc.id, exists, tc.kept, err)
		}
	}
	ageMaintenanceForRetention(t, h, p, 91)
	res, err := h.st.Purge(h.ctx, h.inv.Scope, maintenanceRetentionPolicy)
	if err != nil || res.InvestigationsPurged != 1 {
		t.Fatalf("expired unsaved enqueue never aged out: %+v %v", res, err)
	}
	runMaintenanceCleaner(t, h)
	var exists bool
	if err := h.pool.QueryRow(h.ctx, `SELECT EXISTS(SELECT 1 FROM sage.action_queue WHERE id=$1)`, p.QueueID).Scan(&exists); err != nil || exists {
		t.Fatalf("orphan queue lifetime did not end at cascade: kept=%v err=%v", exists, err)
	}
}
