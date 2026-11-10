package action

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// strand emulates the first commit of a run (RunApproved's executing, or
// Rollback's rolling_back) followed by a crash before anything else ran.
func (h *maintenanceHarness) strand(t *testing.T, id sre.UUID, state string) {
	t.Helper()
	_, err := h.m.mutate(h.ctx, id, "user:1", func(q *MaintenanceProposal, _ sre.Emit) error {
		q.State = state
		if state == "executing" {
			q.ApprovedBy = 1
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// age moves the proposal's last change to at.
func (h *maintenanceHarness) age(t *testing.T, id sre.UUID, at time.Time) {
	t.Helper()
	_, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_maintenance_proposals SET updated_at=$2
		WHERE id=$1`, string(id), at)
	if err != nil {
		t.Fatal(err)
	}
}

// pastBound ages the proposal one second past the stranded bound on the
// database clock, which the sweeper ages proposals by.
func (h *maintenanceHarness) pastBound(t *testing.T, id sre.UUID) {
	t.Helper()
	h.ageBy(t, id, h.m.strandedAfter()+time.Second)
}

// failLeaving injects a control-database failure into any update that moves
// the proposal out of from. The returned func removes it.
func (h *maintenanceHarness) failLeaving(t *testing.T, id sre.UUID, from string) func() {
	t.Helper()
	fn := "fail_" + strings.ReplaceAll(string(sre.NewUUID()), "-", "")[:12]
	for _, sql := range []string{`CREATE FUNCTION public.` + fn + `() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.id::text = TG_ARGV[0] AND OLD.state = TG_ARGV[1] AND NEW.state <> OLD.state
		THEN RAISE EXCEPTION 'injected control-database failure'; END IF;
		RETURN NEW; END $$`,
		`CREATE TRIGGER ` + fn + ` BEFORE UPDATE ON sage.sre_maintenance_proposals
		FOR EACH ROW EXECUTE FUNCTION public.` + fn + `('` + string(id) + `','` + from + `')`,
	} {
		if _, err := h.pool.Exec(h.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	drop := func() {
		ctx := context.Background()
		_, _ = h.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+fn+" ON sage.sre_maintenance_proposals")
		_, _ = h.pool.Exec(ctx, "DROP FUNCTION IF EXISTS public."+fn+"()")
	}
	t.Cleanup(drop)
	return drop
}

// objectOID is the OID of the proposal's deterministic statistics name, 0
// when absent.
func (h *maintenanceHarness) objectOID(t *testing.T, p MaintenanceProposal) uint32 {
	t.Helper()
	var oid uint32
	err := h.pool.QueryRow(h.ctx, `SELECT COALESCE(max(oid::bigint),0) FROM pg_statistic_ext
		WHERE stxname=$1`, "sage_stx_"+strings.ReplaceAll(string(p.ID), "-", "")).Scan(&oid)
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func (h *maintenanceHarness) state(t *testing.T, id sre.UUID) MaintenanceProposal {
	t.Helper()
	p, err := h.m.Get(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// settleEvents counts the proposal's reconciliation events.
func (h *maintenanceHarness) settleEvents(t *testing.T, id sre.UUID) int {
	t.Helper()
	evs, err := h.st.Events(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["proposal_id"] == string(id) && payload["reconciled"] == true {
			n++
		}
	}
	return n
}

func (h *maintenanceHarness) forwardActions(t *testing.T, p MaintenanceProposal) int {
	t.Helper()
	var n int
	err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM sage.action_log WHERE sql_executed=$1`,
		p.SQL).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// crashAfterExecutor runs an approved proposal whose result record fails, so
// the object exists and the proposal is left executing.
func (h *maintenanceHarness) crashAfterExecutor(t *testing.T) MaintenanceProposal {
	t.Helper()
	p := h.request(t)
	a := h.approve(t, p)
	drop := h.failLeaving(t, p.ID, "executing")
	if _, err := h.exec.RunApprovedAction(h.ctx, a, 1); err == nil ||
		!strings.Contains(err.Error(), "injected control-database failure") {
		t.Fatalf("record failure not reported: %v", err)
	}
	drop()
	got := h.state(t, p.ID)
	if got.State != "executing" || got.ActionLogID != 0 || h.objectOID(t, got) == 0 {
		t.Fatalf("crash fixture: %+v object=%d", got, h.objectOID(t, got))
	}
	return got
}

func TestMaintenanceReconcileSettlesLostRecordAsUncertain(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.crashAfterExecutor(t)
	oid := h.objectOID(t, p)
	h.pastBound(t, p.ID)
	h.actions.SetMaintenanceService(h.m)
	if err := h.actions.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	var state string // CHECK-10
	err := h.pool.QueryRow(h.ctx, `SELECT proposal->>'state' FROM sage.sre_maintenance_proposals
		WHERE id=$1`, string(p.ID)).Scan(&state)
	if err != nil || state != "uncertain" {
		t.Fatalf("CHECK-10: state %q %v", state, err)
	}
	got := h.state(t, p.ID)
	if got.ActionLogID <= 0 || got.StatisticsOID != oid || got.Verification != "inconclusive" {
		t.Fatalf("reconciled identity: %+v want oid %d", got, oid)
	}
	var queueAction int64
	var queueVerify string
	err = h.pool.QueryRow(h.ctx, `SELECT COALESCE(action_log_id,0),COALESCE(verification_status,'')
		FROM sage.action_queue WHERE id=$1`, got.QueueID).Scan(&queueAction, &queueVerify)
	if err != nil || queueAction != got.ActionLogID || queueVerify != "inconclusive" {
		t.Fatalf("queue: %d %q %v", queueAction, queueVerify, err)
	}
	if n := h.settleEvents(t, p.ID); n != 1 {
		t.Fatalf("settle events: %d", n)
	}
	if n := h.forwardActions(t, p); n != 1 { // CHECK-12
		t.Fatalf("CHECK-12: forward actions %d", n)
	}
	got, err = h.m.Rollback(h.ctx, p.ID, "remove reconciled object", 1) // CHECK-11
	if err != nil || got.State != "rolled_back" || h.exists(t, got) {
		t.Fatalf("CHECK-11: rollback %+v %v", got, err)
	}
	h.pastBound(t, p.ID)
	if err = h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if again := h.state(t, p.ID); again.State != "rolled_back" || h.settleEvents(t, p.ID) != 1 {
		t.Fatalf("settled proposal touched: %+v", again)
	}
	if err = h.st.VerifyEvents(h.ctx, h.inv.Scope, h.inv.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceReconcileCrashBeforeExecutorSettlesFailed(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.request(t)
	h.approve(t, p)
	h.strand(t, p.ID, "executing")
	h.pastBound(t, p.ID)
	if err := h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	got := h.state(t, p.ID)
	if got.State != "failed" || got.ActionLogID != 0 || got.StatisticsOID != 0 ||
		got.Verification != "failed" || h.exists(t, got) || h.forwardActions(t, got) != 0 {
		t.Fatalf("crash before executor: %+v", got)
	}
	if _, err := h.m.Rollback(h.ctx, p.ID, "nothing to remove", 1); !errors.Is(err,
		ErrProposalState) {
		t.Fatalf("rollback of failed proposal: %v", err)
	}
	if n := h.settleEvents(t, p.ID); n != 1 {
		t.Fatalf("settle events: %d", n)
	}
}

func (h *maintenanceHarness) executed(t *testing.T) MaintenanceProposal {
	t.Helper()
	p := h.request(t)
	if _, err := h.exec.RunApprovedAction(h.ctx, h.approve(t, p), 1); err != nil {
		t.Fatal(err)
	}
	p = h.state(t, p.ID)
	if p.State != "executed" || p.StatisticsOID == 0 {
		t.Fatalf("executed fixture: %+v", p)
	}
	return p
}

func (h *maintenanceHarness) actionOutcome(t *testing.T, id int64) string {
	t.Helper()
	var outcome string
	err := h.pool.QueryRow(h.ctx, `SELECT outcome FROM sage.action_log WHERE id=$1`, id).
		Scan(&outcome)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestMaintenanceReconcileRollbackCrash(t *testing.T) {
	t.Run("after the drop committed", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		p := h.executed(t)
		h.strand(t, p.ID, "rolling_back")
		err := h.exec.RollbackSREStatistics(h.ctx, p.ActionLogID, p.SQL, p.Target,
			p.StatisticsOID, "crash fixture", 1)
		if err != nil || h.exists(t, p) {
			t.Fatalf("drop fixture: %v", err)
		}
		h.pastBound(t, p.ID)
		if err = h.m.Reconcile(h.ctx); err != nil {
			t.Fatal(err)
		}
		got := h.state(t, p.ID)
		if got.State != "rolled_back" || got.Verification != "object_removed" ||
			got.ActionLogID != p.ActionLogID || got.StatisticsOID != p.StatisticsOID {
			t.Fatalf("after drop: %+v", got)
		}
		if o := h.actionOutcome(t, p.ActionLogID); o != "rolled_back" {
			t.Fatalf("action outcome %q", o)
		}
	})
	t.Run("before the drop", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		p := h.executed(t)
		h.strand(t, p.ID, "rolling_back")
		_, err := h.pool.Exec(h.ctx, `UPDATE sage.action_log SET outcome='rolling_back'
			WHERE id=$1`, p.ActionLogID)
		if err != nil {
			t.Fatal(err)
		}
		h.pastBound(t, p.ID)
		if err = h.m.Reconcile(h.ctx); err != nil {
			t.Fatal(err)
		}
		got := h.state(t, p.ID)
		if got.State != "rollback_failed" || !h.exists(t, got) ||
			h.actionOutcome(t, p.ActionLogID) != "rollback_failed" {
			t.Fatalf("before drop: %+v outcome %s", got, h.actionOutcome(t, p.ActionLogID))
		}
		got, err = h.m.Rollback(h.ctx, p.ID, "retry after reconciliation", 1)
		if err != nil || got.State != "rolled_back" || h.exists(t, got) {
			t.Fatalf("retry: %+v %v", got, err)
		}
	})
}

func TestMaintenanceReconcileBound(t *testing.T) {
	h := newMaintenanceHarness(t)
	// Default safety.ddl_timeout_seconds 300 plus the 60 s grace is one apply
	// timeout; the slot wait and the detached run take one each, plus 1 min.
	if got := h.m.strandedAfter(); got != 13*time.Minute {
		t.Fatalf("stranded bound %v", got)
	}
	p := h.request(t)
	h.strand(t, p.ID, "executing")
	at := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	h.age(t, p.ID, at)
	h.m.Now = func() time.Time { return at.Add(h.m.strandedAfter() - time.Microsecond) }
	if err := h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t, p.ID); got.State != "executing" || h.settleEvents(t, p.ID) != 0 {
		t.Fatalf("younger than the bound was swept: %+v", got)
	}
	h.m.Now = func() time.Time { return at.Add(h.m.strandedAfter()) }
	if err := h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t, p.ID); got.State != "failed" {
		t.Fatalf("exactly at the bound was not swept: %+v", got)
	}
}

func TestMaintenanceReconcileRefusesUnprovenObjects(t *testing.T) {
	t.Run("same name on another table", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		p := h.request(t)
		h.strand(t, p.ID, "executing")
		other := h.table + "_other"
		name := "sage_stx_" + strings.ReplaceAll(string(p.ID), "-", "")
		for _, sql := range []string{"CREATE TABLE public." + other + "(a int,b int)",
			"CREATE STATISTICS public." + name + " ON a, b FROM public." + other} {
			if _, err := h.pool.Exec(h.ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			_, _ = h.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+other)
		})
		h.assertUnproven(t, p, "another table")
	})
	t.Run("object without an action record", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		p := h.request(t)
		h.strand(t, p.ID, "executing")
		if _, err := h.pool.Exec(h.ctx, p.SQL); err != nil {
			t.Fatal(err)
		}
		h.assertUnproven(t, p, "no action record")
	})
}

// assertUnproven reconciles p and expects uncertain with no claimed object,
// a refused rollback and the object left in place.
func (h *maintenanceHarness) assertUnproven(t *testing.T, p MaintenanceProposal, why string) {
	t.Helper()
	before := h.objectOID(t, p)
	h.pastBound(t, p.ID)
	if err := h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	got := h.state(t, p.ID)
	if got.State != "uncertain" || got.StatisticsOID != 0 || !strings.Contains(got.Detail, why) {
		t.Fatalf("unproven object claimed: %+v", got)
	}
	if _, err := h.m.Rollback(h.ctx, p.ID, "must not drop", 1); !errors.Is(err,
		ErrProposalState) {
		t.Fatalf("rollback of unproven object: %v", err)
	}
	if after := h.objectOID(t, p); before == 0 || after != before {
		t.Fatalf("object changed: before %d after %d", before, after)
	}
}

func TestMaintenanceReconcileRacesLateRecord(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.crashAfterExecutor(t)
	oid := h.objectOID(t, p)
	var actionID int64
	err := h.pool.QueryRow(h.ctx, `SELECT id FROM sage.action_log WHERE sql_executed=$1`,
		p.SQL).Scan(&actionID)
	if err != nil {
		t.Fatal(err)
	}
	h.pastBound(t, p.ID)
	var wg sync.WaitGroup
	start := make(chan struct{})
	var sweepErr, recordErr error
	wg.Add(2)
	go func() { defer wg.Done(); <-start; sweepErr = h.m.Reconcile(h.ctx) }()
	go func() {
		defer wg.Done()
		<-start
		_, recordErr = h.m.record(h.ctx, p.ID, "user:1", actionID, oid, nil)
	}()
	close(start)
	wg.Wait()
	if sweepErr != nil {
		t.Fatalf("sweeper: %v", sweepErr)
	}
	got := h.state(t, p.ID)
	switch {
	case recordErr == nil && got.State == "executed" && h.settleEvents(t, p.ID) == 0:
	case errors.Is(recordErr, ErrProposalState) && got.State == "uncertain" &&
		h.settleEvents(t, p.ID) == 1:
	default:
		t.Fatalf("no single winner: record %v state %+v events %d", recordErr, got,
			h.settleEvents(t, p.ID))
	}
	if got.ActionLogID != actionID || got.StatisticsOID != oid {
		t.Fatalf("inconsistent identity: %+v want %d/%d", got, actionID, oid)
	}
}

func TestMaintenanceReconcileControlErrorLeavesState(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.request(t)
	h.strand(t, p.ID, "executing")
	h.pastBound(t, p.ID)
	drop := h.failLeaving(t, p.ID, "executing")
	h.actions.SetMaintenanceService(h.m)
	err := h.actions.Tick(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "injected control-database failure") ||
		!strings.Contains(err.Error(), string(p.ID)) {
		t.Fatalf("control error not returned with context: %v", err)
	}
	if got := h.state(t, p.ID); got.State != "executing" || h.settleEvents(t, p.ID) != 0 {
		t.Fatalf("state changed on control error: %+v", got)
	}
	drop()
	if err = h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t, p.ID); got.State != "failed" {
		t.Fatalf("retry after control error: %+v", got)
	}
}

func TestMaintenanceReconcileWaitsForTheTableLock(t *testing.T) {
	h := newMaintenanceHarness(t)
	p := h.crashAfterExecutor(t)
	h.pastBound(t, p.ID)
	// An in-flight executor DDL holds this lock on the bound table.
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(h.ctx, "LOCK TABLE public."+h.table+
		" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err = h.m.Reconcile(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "lock bound table") {
		t.Fatalf("reconcile under a held table lock: %v", err)
	}
	if waited := time.Since(began); waited > 8*time.Second {
		t.Fatalf("lock wait unbounded: %v", waited)
	}
	if got := h.state(t, p.ID); got.State != "executing" || h.settleEvents(t, p.ID) != 0 {
		t.Fatalf("settled without the table lock: %+v", got)
	}
	if err = tx.Rollback(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err = h.m.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t, p.ID); got.State != "uncertain" || got.StatisticsOID == 0 {
		t.Fatalf("after the lock was released: %+v", got)
	}
}
