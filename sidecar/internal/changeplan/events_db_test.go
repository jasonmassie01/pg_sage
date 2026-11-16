package changeplan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// plan-01b: every plan write records its event in the same transaction,
// and the events are an append-only audit chain.

// appendInTx writes e in its own transaction, the way the engine records a
// step transition next to the step's state change.
func appendInTx(t *testing.T, pool *pgxpool.Pool, e Event) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := AppendEvent(ctx, tx, e)
	if err != nil {
		t.Fatalf("append %s: %v", e.Kind, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", e.Kind, err)
	}
	return id
}

func mustEvents(t *testing.T, s *Store, planID int64) []Event {
	t.Helper()
	evs, err := s.Events(context.Background(), planID)
	if err != nil {
		t.Fatalf("events of plan %d: %v", planID, err)
	}
	return evs
}

func eventKinds(evs []Event) []EventKind {
	out := make([]EventKind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func verifyPlanEvents(t *testing.T, pool *pgxpool.Pool) auditchain.Report {
	t.Helper()
	rep, err := auditchain.Verify(context.Background(), pool, auditchain.PlanEvents,
		auditchain.Window{})
	if err != nil {
		t.Fatalf("verify plan_events: %v", err)
	}
	return rep
}

// Happy path: create, approve, two step events and a verdict are five
// links, in commit order, and the chain verifies.
func TestEvents_PlanLifecycleIsFiveLinksInCommitOrder(t *testing.T) {
	pool := planPool(t, "plan_events_happy")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	step := p.Steps[0].ID
	appendInTx(t, pool, Event{PlanID: p.ID, StepID: &step, Kind: EventStepStarted,
		PlanHash: p.PlanHash, Actor: "pg_sage"})
	appendInTx(t, pool, Event{PlanID: p.ID, StepID: &step, Kind: EventStepExecuted,
		PlanHash: p.PlanHash, Actor: "pg_sage", Detail: map[string]any{"action_log_id": 5}})
	appendInTx(t, pool, Event{PlanID: p.ID, StepID: &step, Kind: EventVerdict,
		PlanHash: p.PlanHash, Actor: "pg_sage", Detail: map[string]any{"verdict": "improved"}})
	evs := mustEvents(t, s, p.ID)
	want := []EventKind{EventCreated, EventApproved, EventStepStarted, EventStepExecuted,
		EventVerdict}
	if got := eventKinds(evs); !slices.Equal(got, want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	if evs[0].Actor != "alice" || evs[1].Actor != "user:7" || evs[0].StepID != nil ||
		evs[2].StepID == nil || *evs[2].StepID != step {
		t.Fatalf("actors and steps = %+v", evs)
	}
	for _, e := range evs {
		if e.PlanHash != p.PlanHash || e.PlanID != p.ID || e.CreatedAt.IsZero() {
			t.Fatalf("event %+v, want plan %d at hash %s", e, p.ID, p.PlanHash)
		}
	}
	if evs[4].Detail["verdict"] != "improved" || evs[0].Detail["status"] != "proposed" {
		t.Fatalf("details = %v / %v", evs[4].Detail, evs[0].Detail)
	}
	var prev int64
	for _, e := range evs {
		links, err := auditchain.LinksForRow(ctx, pool, "plan_events", e.ID)
		if err != nil || len(links) != 1 || links[0].Op != "I" || links[0].Seq <= prev {
			t.Fatalf("links of event %d = %+v %v, want one insert after seq %d", e.ID,
				links, err, prev)
		}
		prev = links[0].Seq
	}
	rep := verifyPlanEvents(t, pool)
	if !rep.OK() || rep.Links != 5 || rep.HeadSeq != 5 {
		t.Fatalf("plan_events report = %+v", rep)
	}
}

// Edits and voids are recorded with the actor the caller names, and an
// unnamed caller is recorded as unattributed rather than as pg_sage.
func TestEvents_EditVoidAndReapproveAreRecorded(t *testing.T) {
	pool := planPool(t, "plan_events_edit")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, p.ID, p.PlanHash, 8); err != nil {
		t.Fatal(err) // the same hash again: the live approval, no new event
	}
	step := p.Steps[1]
	step.ProposedSQL = "CREATE INDEX CONCURRENTLY idx_b2 ON public.orders (created_at)"
	edited, err := s.UpdateStep(WithActor(ctx, "bob"), p.ID, step, newFakeResolver())
	if err != nil {
		t.Fatalf("update step: %v", err)
	}
	if _, err := s.SetEdges(ctx, p.ID, []Edge{{From: 1, To: 3}}, newFakeResolver()); err != nil {
		t.Fatalf("set edges: %v", err)
	}
	if err := s.Void(WithActor(ctx, "carol"), p.ID, "operator withdrew it"); err != nil {
		t.Fatalf("void: %v", err)
	}
	evs := mustEvents(t, s, p.ID)
	want := []EventKind{EventCreated, EventApproved, EventEdited, EventEdited, EventVoided}
	if got := eventKinds(evs); !slices.Equal(got, want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	ed := evs[2]
	if ed.Actor != "bob" || ed.StepID == nil || *ed.StepID != step.ID ||
		ed.PlanHash != edited.PlanHash || ed.Detail["voided_approval"] != true ||
		ed.Detail["status"] != "proposed" {
		t.Fatalf("edit event = %+v", ed)
	}
	if evs[3].Actor != Unattributed || evs[3].StepID != nil ||
		evs[3].Detail["voided_approval"] != false {
		t.Fatalf("edge event = %+v", evs[3])
	}
	if evs[4].Actor != "carol" || evs[4].Detail["reason"] != "operator withdrew it" {
		t.Fatalf("void event = %+v", evs[4])
	}
	if rep := verifyPlanEvents(t, pool); !rep.OK() || rep.Links != 5 {
		t.Fatalf("plan_events report = %+v", rep)
	}
}

// Invalid input: the database refuses a kind outside its CHECK, and
// AppendEvent refuses a bad event before it reaches the database.
func TestEvents_InvalidEventsAreRefused(t *testing.T) {
	pool := planPool(t, "plan_events_invalid")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	_, err := pool.Exec(ctx, `INSERT INTO sage.plan_event (plan_id, kind, plan_hash, actor)
		VALUES ($1, 'exploded', $2, 'x')`, p.ID, p.PlanHash)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("kind outside the CHECK: %v, want a check violation", err)
	}
	cases := []struct {
		name string
		e    Event
		want error
	}{
		{"unknown kind", Event{PlanID: p.ID, Kind: "exploded", PlanHash: p.PlanHash,
			Actor: "x"}, ErrInvalidEvent},
		{"no plan", Event{Kind: EventHalted, PlanHash: p.PlanHash, Actor: "x"},
			ErrInvalidEvent},
		{"bad hash", Event{PlanID: p.ID, Kind: EventHalted, PlanHash: "ABC", Actor: "x"},
			ErrInvalidEvent},
		{"no actor", Event{PlanID: p.ID, Kind: EventHalted, PlanHash: p.PlanHash},
			ErrInvalidEvent},
		{"long actor", Event{PlanID: p.ID, Kind: EventHalted, PlanHash: p.PlanHash,
			Actor: strings.Repeat("a", 201)}, ErrInvalidEvent},
		{"missing plan", Event{PlanID: p.ID + 99, Kind: EventHalted, PlanHash: p.PlanHash,
			Actor: "x"}, ErrNotFound},
	}
	for _, tc := range cases {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = AppendEvent(ctx, tx, tc.e)
		_ = tx.Rollback(ctx)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if evs := mustEvents(t, s, p.ID); len(evs) != 1 {
		t.Fatalf("events after the refusals = %v, want only the create", eventKinds(evs))
	}
}

// failKind makes the database refuse every event of kind, the way a full
// disk or a revoked grant refuses the event insert.
func failKind(t *testing.T, pool *pgxpool.Pool, kind EventKind) {
	t.Helper()
	ctx := context.Background()
	sql := fmt.Sprintf(`CREATE FUNCTION sage.test_fail_event() RETURNS trigger
		LANGUAGE plpgsql AS $fn$ BEGIN IF NEW.kind = '%s' THEN
		RAISE EXCEPTION 'test refuses the event'; END IF; RETURN NEW; END $fn$;
		CREATE TRIGGER test_fail_event BEFORE INSERT ON sage.plan_event
		FOR EACH ROW EXECUTE FUNCTION sage.test_fail_event()`, kind)
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("install the failing trigger: %v", err)
	}
}

// Error propagation: a refused event rolls the state change back, and the
// error names the event's kind.
func TestEvents_AFailedEventRollsBackTheChange(t *testing.T) {
	pool := planPool(t, "plan_events_rollback")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	failKind(t, pool, EventApproved)
	_, err := s.Approve(ctx, p.ID, p.PlanHash, 7)
	if err == nil || !strings.Contains(err.Error(), "approved event") ||
		!strings.Contains(err.Error(), "test refuses the event") {
		t.Fatalf("approve with the event refused: %v, want the approved event named", err)
	}
	if got := mustGet(t, s, p.ID); got.Status != StatusProposed {
		t.Fatalf("status = %s, want proposed: the approval must roll back", got.Status)
	}
	if apps := mustApprovals(t, s, p.ID); len(apps) != 0 {
		t.Fatalf("approvals = %+v, want none", apps)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER test_fail_event ON sage.plan_event;
		DROP FUNCTION sage.test_fail_event()`); err != nil {
		t.Fatal(err)
	}
	failKind(t, pool, EventCreated)
	if _, err := s.Create(ctx, threeStepPlan(), newFakeResolver()); err == nil ||
		!strings.Contains(err.Error(), "created event") {
		t.Fatalf("create with the event refused: %v, want the created event named", err)
	}
	var plans int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.plan`).Scan(&plans); err != nil ||
		plans != 1 {
		t.Fatalf("plans = %d (%v), want only the first: the create must roll back", plans, err)
	}
}

// Concurrency: 20 writers recording events for 5 plans leave a gap-free
// chain with every event linked once.
func TestEvents_ConcurrentWritersLeaveAGapFreeChain(t *testing.T) {
	pool := planPool(t, "plan_events_concurrent")
	s := NewStore(pool)
	ctx := context.Background()
	plans := make([]Plan, 5)
	for i := range plans {
		plans[i] = mustCreate(t, s, threeStepPlan())
	}
	const writers, perWriter = 20, 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				p := plans[(w+i)%len(plans)]
				errs <- writeStepEvent(ctx, pool, p, w, i)
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent event: %v", err)
		}
	}
	want := len(plans) + writers*perWriter
	rep := verifyPlanEvents(t, pool)
	if !rep.OK() || rep.Links != want || rep.HeadSeq != int64(want) ||
		rep.RowsChecked != want {
		t.Fatalf("after %d concurrent events: %+v", writers*perWriter, rep)
	}
	total := 0
	for _, p := range plans {
		total += len(mustEvents(t, s, p.ID))
	}
	if total != want {
		t.Fatalf("events listed = %d, want %d", total, want)
	}
}

// writeStepEvent locks the plan's first step, changes its state and
// records the transition, in one transaction.
func writeStepEvent(ctx context.Context, pool *pgxpool.Pool, p Plan, w, i int) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		step := p.Steps[0].ID
		if _, err := tx.Exec(ctx, `UPDATE sage.plan_step SET state_reason = $2
			WHERE id = $1`, step, fmt.Sprintf("w%d-%d", w, i)); err != nil {
			return fmt.Errorf("update step %d: %w", step, err)
		}
		_, err := AppendEvent(ctx, tx, Event{PlanID: p.ID, StepID: &step,
			Kind: EventStepStarted, PlanHash: p.PlanHash, Actor: "pg_sage",
			Detail: map[string]any{"writer": w, "i": i}})
		return err
	})
}
