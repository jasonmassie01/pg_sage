package changeplan

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/changeplan"))
}

// planPool is a fresh database bootstrapped twice (plan-01a: the second
// boot must be clean too).
func planPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for run := range 2 {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", run, err)
		}
	}
	return pool
}

func mustCreate(t *testing.T, s *Store, p Plan) Plan {
	t.Helper()
	got, err := s.Create(context.Background(), p, newFakeResolver())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return got
}

// expectedStored is p as the store returns it after Create: defaults
// applied, ids and times from got.
func expectedStored(p, got Plan) Plan {
	p.ID, p.CreatedAt, p.UpdatedAt = got.ID, got.CreatedAt, got.UpdatedAt
	p.PlanHash = PlanHash(p)
	for i := range p.Steps {
		p.Steps[i] = p.Steps[i].WithDefaults()
		if i < len(got.Steps) {
			p.Steps[i].ID = got.Steps[i].ID
		}
	}
	return p
}

func TestStore_RoundTripIsFieldForField(t *testing.T) {
	s := NewStore(planPool(t, "plan_roundtrip"))
	ctx := context.Background()
	in := threeStepPlan()
	in.DatabaseID = ptr(3)
	in.Model, in.PromptVersion = "model-x", "p-1"
	created := mustCreate(t, s, in)
	if created.ID <= 0 || len(created.Steps) != 3 || created.Steps[0].ID <= 0 ||
		created.CreatedAt.IsZero() || created.PlanHash != PlanHash(in) {
		t.Fatalf("created %+v, want ids, times and the plan hash", created)
	}
	got, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := expectedStored(in, got)
	if !reflect.DeepEqual(got.Steps, want.Steps) {
		for i := range got.Steps {
			if !reflect.DeepEqual(got.Steps[i], want.Steps[i]) {
				t.Errorf("step %d:\n got %+v\nwant %+v", i+1, got.Steps[i], want.Steps[i])
			}
		}
	}
	got.Steps, want.Steps = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan:\n got %+v\nwant %+v", got, want)
	}
	if got.Status != StatusProposed || got.Edges[0] != (Edge{From: 1, To: 3}) {
		t.Fatalf("status %s edges %v", got.Status, got.Edges)
	}
}

func TestStore_NilAndEmptyFieldsRoundTrip(t *testing.T) {
	s := NewStore(planPool(t, "plan_nil"))
	p := threeStepPlan()
	p.Scope, p.Edges, p.PackHash = Scope{}, nil, ""
	p.Steps[2].EvidenceIDs, p.Steps[2].FindingID = nil, ptr(int64(13))
	p.Steps[0].Guardrails, p.Steps[0].ExpectedPct = nil, nil
	created := mustCreate(t, s, p)
	got, err := s.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PackHash != "" || len(got.Edges) != 0 || len(got.Scope.Objects) != 0 ||
		got.Steps[0].ExpectedPct != nil || len(got.Steps[0].Guardrails) != 0 ||
		got.Steps[2].EvidenceIDs != nil || got.DatabaseID != nil {
		t.Fatalf("nil fields came back as %+v", got)
	}
	if got.PlanHash != PlanHash(got) {
		t.Fatal("a plan with nil fields does not hash to its stored hash")
	}
}

func TestStore_CreateRefusesAnInvalidPlanAndStoresNothing(t *testing.T) {
	pool := planPool(t, "plan_invalid")
	s := NewStore(pool)
	p := threeStepPlan()
	p.Steps[0].FindingID = nil
	_, err := s.Create(context.Background(), p, newFakeResolver())
	var inv *InvalidPlanError
	if !errors.As(err, &inv) || !errors.Is(err, ErrInvalid) ||
		!hasCode(inv.Problems, CodeEvidenceUnresolved, 1) {
		t.Fatalf("create: %v, want an InvalidPlanError with evidence_unresolved@1", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.plan`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plans stored after a refused create: %d (%v)", n, err)
	}
}

func TestStore_ApproveEditVoidsAndStaleHashIsRefused(t *testing.T) {
	s := NewStore(planPool(t, "plan_states"))
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	first, err := s.Approve(ctx, p.ID, p.PlanHash, 7)
	if err != nil || first.ID <= 0 || first.PlanHash != p.PlanHash || first.ApproverUserID != 7 {
		t.Fatalf("approve: %+v, %v", first, err)
	}
	again, err := s.Approve(ctx, p.ID, p.PlanHash, 8)
	if err != nil || again.ID != first.ID {
		t.Fatalf("second approve with the same hash: %+v %v, want approval %d", again, err,
			first.ID)
	}
	if got := mustGet(t, s, p.ID); got.Status != StatusApproved {
		t.Fatalf("status after approve: %s", got.Status)
	}
	if err := s.Verify(ctx, p.ID); err != nil {
		t.Fatalf("verify after approve: %v", err)
	}
	step := p.Steps[1]
	step.ProposedSQL = "CREATE INDEX CONCURRENTLY idx_b2 ON public.orders (created_at)"
	edited, err := s.UpdateStep(ctx, p.ID, step, newFakeResolver())
	if err != nil {
		t.Fatalf("update step: %v", err)
	}
	if edited.Status != StatusProposed || edited.PlanHash == p.PlanHash ||
		edited.PlanHash != PlanHash(edited) {
		t.Fatalf("after edit: status %s hash %s (old %s)", edited.Status, edited.PlanHash,
			p.PlanHash)
	}
	apps := mustApprovals(t, s, p.ID)
	if len(apps) != 1 || apps[0].VoidedAt == nil || apps[0].VoidReason != VoidEdited {
		t.Fatalf("approvals after edit: %+v, want the one voided as edited", apps)
	}
	if err := s.Verify(ctx, p.ID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("verify after the edit: %v, want ErrNotApproved", err)
	}
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); !errors.Is(err, ErrStaleHash) ||
		!strings.Contains(err.Error(), "changed after it was shown") {
		t.Fatalf("approve with the old hash: %v, want ErrStaleHash", err)
	}
	if _, err := s.Approve(ctx, p.ID, edited.PlanHash, 7); err != nil {
		t.Fatalf("approve the edited plan: %v", err)
	}
	if live := liveApprovals(t, s, p.ID); live != 1 {
		t.Fatalf("live approvals %d, want 1", live)
	}
}

func TestStore_EdgeChangeVoidsTheApproval(t *testing.T) {
	s := NewStore(planPool(t, "plan_edges"))
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetEdges(ctx, p.ID, []Edge{{From: 1, To: 2}, {From: 2, To: 3}},
		newFakeResolver())
	if err != nil {
		t.Fatalf("set edges: %v", err)
	}
	if got.Status != StatusProposed || len(got.Edges) != 2 || got.Edges[0] != (Edge{1, 2}) {
		t.Fatalf("after the edge change: %s %v", got.Status, got.Edges)
	}
	if live := liveApprovals(t, s, p.ID); live != 0 {
		t.Fatalf("live approvals after an edge change: %d", live)
	}
	_, err = s.SetEdges(ctx, p.ID, []Edge{{From: 1, To: 2}, {From: 2, To: 1}}, newFakeResolver())
	var inv *InvalidPlanError
	if !errors.As(err, &inv) || !hasCode(inv.Problems, CodeEdgeCycle, 1) {
		t.Fatalf("a cyclic edge set: %v, want edge_cycle", err)
	}
	if after := mustGet(t, s, p.ID); len(after.Edges) != 2 || after.Edges[1] != (Edge{2, 3}) {
		t.Fatalf("a refused edge set changed the edges: %v", after.Edges)
	}
}

// CHECK-02: a row edited by direct SQL makes Verify report hash_mismatch.
func TestStore_DirectSQLEditIsAHashMismatch(t *testing.T) {
	pool := planPool(t, "plan_direct")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	if err := s.Verify(ctx, p.ID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("verify before approval: %v, want ErrNotApproved", err)
	}
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"step sql": `UPDATE sage.plan_step SET proposed_sql = proposed_sql || ' '
		              WHERE plan_id = $1 AND ordinal = 2`,
		"guardrail": `UPDATE sage.plan_step SET guardrails = '[]'
		              WHERE plan_id = $1 AND ordinal = 1`,
		"edge": `DELETE FROM sage.plan_step_edge WHERE plan_id = $1
		          AND to_step_id = (SELECT id FROM sage.plan_step WHERE plan_id = $1
		                             AND ordinal = 3) AND from_step_id = (SELECT id FROM
		                             sage.plan_step WHERE plan_id = $1 AND ordinal = 1)`,
		"goal": `UPDATE sage.plan SET goal = goal || '!' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if tag, err := tx.Exec(ctx, sql, p.ID); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("direct edit: %v (%d rows)", err, tag.RowsAffected())
			}
			err = verifyOn(ctx, tx, p.ID)
			if !errors.Is(err, ErrHashMismatch) || !strings.Contains(err.Error(), "hash_mismatch") {
				t.Fatalf("verify after a direct %s edit: %v, want hash_mismatch", name, err)
			}
		})
	}
	// The runtime columns are outside the hash.
	if _, err := pool.Exec(ctx, `UPDATE sage.plan_step SET state = 'kept', verdict = 'improved',
		queue_id = 5 WHERE plan_id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(ctx, p.ID); err != nil {
		t.Fatalf("verify after a runtime update: %v, want nil", err)
	}
}

func TestStore_RunningAndTerminalPlansAreNotEditable(t *testing.T) {
	pool := planPool(t, "plan_locked")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	for _, st := range []Status{StatusRunning, StatusCompleted, StatusVoided} {
		if _, err := pool.Exec(ctx, `UPDATE sage.plan SET status = $2 WHERE id = $1`, p.ID,
			string(st)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateStep(ctx, p.ID, p.Steps[0], newFakeResolver()); !errors.Is(err,
			ErrNotEditable) {
			t.Errorf("edit a %s plan: %v, want ErrNotEditable", st, err)
		}
		if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); !errors.Is(err, ErrNotApprovable) {
			t.Errorf("approve a %s plan: %v, want ErrNotApprovable", st, err)
		}
	}
}

func TestStore_VoidEndsThePlanAndItsApproval(t *testing.T) {
	s := NewStore(planPool(t, "plan_void"))
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.Void(ctx, p.ID, ""); err == nil {
		t.Fatal("void with no reason was accepted")
	}
	if err := s.Void(ctx, p.ID, "operator withdrew it"); err != nil {
		t.Fatalf("void: %v", err)
	}
	if got := mustGet(t, s, p.ID); got.Status != StatusVoided {
		t.Fatalf("status after void: %s", got.Status)
	}
	apps := mustApprovals(t, s, p.ID)
	if len(apps) != 1 || apps[0].VoidReason != "operator withdrew it" {
		t.Fatalf("approvals after void: %+v", apps)
	}
	if err := s.Void(ctx, p.ID, "again"); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("void a voided plan: %v, want ErrNotEditable", err)
	}
}

func TestStore_NotFoundAndBadInput(t *testing.T) {
	s := NewStore(planPool(t, "plan_missing"))
	ctx := context.Background()
	if _, err := s.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get 999: %v", err)
	}
	if _, err := s.Approve(ctx, 999, strings.Repeat("a", 64), 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve 999: %v", err)
	}
	if err := s.Verify(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verify 999: %v", err)
	}
	p := mustCreate(t, s, threeStepPlan())
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 0); err == nil {
		t.Fatal("approve with no user was accepted")
	}
	step := p.Steps[0]
	step.Ordinal = 9
	if _, err := s.UpdateStep(ctx, p.ID, step, newFakeResolver()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update a step the plan lacks: %v, want ErrNotFound", err)
	}
	if _, err := s.List(ctx, ListFilter{Limit: 0}); err == nil {
		t.Fatal("list with limit 0 was accepted")
	}
}

func TestStore_ListFiltersByStatusNewestFirst(t *testing.T) {
	s := NewStore(planPool(t, "plan_list"))
	ctx := context.Background()
	a := mustCreate(t, s, threeStepPlan())
	b := mustCreate(t, s, threeStepPlan())
	if _, err := s.Approve(ctx, b.ID, b.PlanHash, 7); err != nil {
		t.Fatal(err)
	}
	all, err := s.List(ctx, ListFilter{Limit: 10})
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("list: %+v %v, want b then a", all, err)
	}
	approved, err := s.List(ctx, ListFilter{Status: StatusApproved, Limit: 10})
	if err != nil || len(approved) != 1 || approved[0].ID != b.ID {
		t.Fatalf("list approved: %+v %v", approved, err)
	}
	one, err := s.List(ctx, ListFilter{Limit: 1})
	if err != nil || len(one) != 1 {
		t.Fatalf("list limit 1: %+v %v", one, err)
	}
}

func TestStore_WritesOnAClosedPoolNameThePlan(t *testing.T) {
	pool := planPool(t, "plan_closed")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	pool.Close()
	want := "plan " + strconv.FormatInt(p.ID, 10)
	if _, err := s.UpdateStep(ctx, p.ID, p.Steps[0], newFakeResolver()); err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Fatalf("update on a closed pool: %v, want it to name %q", err, want)
	}
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Fatalf("approve on a closed pool: %v, want it to name %q", err, want)
	}
	if _, err := s.Create(ctx, threeStepPlan(), newFakeResolver()); err == nil ||
		!strings.Contains(err.Error(), "create plan") {
		t.Fatalf("create on a closed pool: %v", err)
	}
}

// Two sessions edit and approve one plan at once, 50 times: either the
// approval is voided by the edit or refused as stale; never a live
// approval of a hash the rows no longer have.
func TestStore_ConcurrentEditAndApproveNeverLeaveAStaleApproval(t *testing.T) {
	s := NewStore(planPool(t, "plan_race"))
	ctx := context.Background()
	for round := range 50 {
		p := mustCreate(t, s, threeStepPlan())
		step := p.Steps[0]
		step.ProposedSQL = "CREATE INDEX CONCURRENTLY idx_r ON public.orders (customer_id)"
		var wg sync.WaitGroup
		var editErr, approveErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, editErr = s.UpdateStep(ctx, p.ID, step, newFakeResolver())
		}()
		go func() {
			defer wg.Done()
			_, approveErr = s.Approve(ctx, p.ID, p.PlanHash, 7)
		}()
		wg.Wait()
		if editErr != nil {
			t.Fatalf("round %d: edit: %v", round, editErr)
		}
		apps := mustApprovals(t, s, p.ID)
		switch {
		case approveErr == nil:
			if len(apps) != 1 || apps[0].VoidReason != VoidEdited {
				t.Fatalf("round %d: approved first, approvals %+v, want it voided", round, apps)
			}
		case errors.Is(approveErr, ErrStaleHash):
			if len(apps) != 0 {
				t.Fatalf("round %d: refused as stale, yet approvals %+v", round, apps)
			}
		default:
			t.Fatalf("round %d: approve: %v", round, approveErr)
		}
		if err := s.Verify(ctx, p.ID); !errors.Is(err, ErrNotApproved) {
			t.Fatalf("round %d: verify %v, want no live approval", round, err)
		}
	}
}

func TestSchema_RefusesCrossPlanEdgesAndTwoLiveApprovals(t *testing.T) {
	pool := planPool(t, "plan_constraints")
	s := NewStore(pool)
	ctx := context.Background()
	a := mustCreate(t, s, threeStepPlan())
	b := mustCreate(t, s, threeStepPlan())
	_, err := pool.Exec(ctx, `INSERT INTO sage.plan_step_edge (plan_id, from_step_id,
		to_step_id) VALUES ($1, $2, $3)`, a.ID, a.Steps[0].ID, b.Steps[1].ID)
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("an edge to another plan's step: %v, want a foreign key violation", err)
	}
	if _, err := s.Approve(ctx, a.ID, a.PlanHash, 7); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.plan_approval (plan_id, plan_hash,
		approver_user_id) VALUES ($1, $2, 8)`, a.ID, a.PlanHash)
	if err == nil || !strings.Contains(err.Error(), "plan_approval_live") {
		t.Fatalf("a second live approval: %v, want the live-approval key", err)
	}
	for name, sql := range map[string]string{
		"bad origin":  `UPDATE sage.plan SET origin = 'oracle' WHERE id = $1`,
		"bad status":  `UPDATE sage.plan SET status = 'done' WHERE id = $1`,
		"long goal":   `UPDATE sage.plan SET goal = repeat('g', 501) WHERE id = $1`,
		"upper hash":  `UPDATE sage.plan SET pack_hash = upper(pack_hash) WHERE id = $1`,
		"bad state":   `UPDATE sage.plan_step SET state = 'done' WHERE plan_id = $1`,
		"deadline 0":  `UPDATE sage.plan_step SET deadline_minutes = 0 WHERE plan_id = $1`,
		"bad action":  `UPDATE sage.plan_step SET on_expired = 'shrug' WHERE plan_id = $1`,
		"bad verdict": `UPDATE sage.plan_step SET verdict = 'neutral' WHERE plan_id = $1`,
		"ordinal 51":  `UPDATE sage.plan_step SET ordinal = 51 WHERE plan_id = $1 AND ordinal = 1`,
		"self edge": `UPDATE sage.plan_step_edge SET to_step_id = from_step_id
		               WHERE plan_id = $1`,
		"void no text": `UPDATE sage.plan_approval SET voided_at = now() WHERE plan_id = $1`,
	} {
		target := b.ID
		if name == "void no text" {
			target = a.ID // only a has an approval row
		}
		if _, err := pool.Exec(ctx, sql, target); err == nil {
			t.Errorf("%s: accepted, want a constraint violation", name)
		}
	}
}

func mustGet(t *testing.T, s *Store, id int64) Plan {
	t.Helper()
	p, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get plan %d: %v", id, err)
	}
	return p
}

func mustApprovals(t *testing.T, s *Store, id int64) []Approval {
	t.Helper()
	apps, err := s.Approvals(context.Background(), id)
	if err != nil {
		t.Fatalf("approvals of plan %d: %v", id, err)
	}
	return apps
}

func liveApprovals(t *testing.T, s *Store, id int64) int {
	n := 0
	for _, a := range mustApprovals(t, s, id) {
		if a.VoidedAt == nil {
			n++
		}
	}
	return n
}
