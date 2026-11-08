package sre

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Attempts (sre-01): every automatic trigger carries an idempotency key
// (incident:<id>, finding:<id>, ...). When the key's newest run expired
// in the queue (queue_expired) or on stale evidence (stale_evidence) and
// the trigger is still open, the next poll starts attempt n+1, which
// supersedes the expired run. Operators' keys keep HTTP idempotency, a
// paused run is never expired by a poll, and every expiry is fenced and
// on the event chain.

// keyedStart is a system trigger with an idempotency key, as the
// incident and finding trigger sources build them.
func keyedStart(scope Scope, subject string) StartRequest {
	req := lockStart(scope, subject)
	req.IncidentID = subject
	req.IdempotencyKey = "incident:" + subject
	return req
}

// setQueueExpiry moves an investigation's queue expiry to now + in (a
// negative in is in the past). created_at moves an hour back so the
// table's expires_at > created_at check still holds.
func setQueueExpiry(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	inv Investigation, in time.Duration) {
	t.Helper()
	tag, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET created_at = LEAST(created_at, clock_timestamp() - interval '1 hour'),
		    expires_at = clock_timestamp() + make_interval(secs => $4)
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3`,
		string(inv.Scope.DeploymentID), string(inv.Scope.DatabaseID), string(inv.ID),
		in.Seconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("set queue expiry of %s: %v (%d rows)", inv.ID, err, tag.RowsAffected())
	}
}

// eventsOf returns the investigation's events with their decoded payloads.
func eventsOf(t *testing.T, ctx context.Context, st *PostgresStore,
	inv Investigation) ([]Event, []map[string]any) {
	t.Helper()
	events, err := st.Events(ctx, inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("events of %s: %v", inv.ID, err)
	}
	payloads := make([]map[string]any, len(events))
	for i, e := range events {
		if err := json.Unmarshal(e.Payload, &payloads[i]); err != nil {
			t.Fatalf("event %d payload %s: %v", e.Sequence, e.Payload, err)
		}
	}
	return events, payloads
}

func countKeyRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope Scope,
	key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND idempotency_key = $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), key).Scan(&n); err != nil {
		t.Fatalf("count rows of key %s: %v", key, err)
	}
	return n
}

// CHECK-31 (state transition, fails before sre-01): a keyed trigger is
// queued and expires; the next poll creates attempt 2, which supersedes
// it, and the created event records the attempt.
func TestStore_ExpiredKeyedTriggerStartsAttempt2(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	old, created, err := st.Create(ctx, keyedStart(scope, "att-1"))
	if err != nil || !created || old.Attempt != 1 || old.Supersedes != "" {
		t.Fatalf("first create = attempt %d supersedes %q created=%v err=%v",
			old.Attempt, old.Supersedes, created, err)
	}
	setQueueExpiry(t, ctx, pool, old, -time.Millisecond)
	next, created, err := st.Create(ctx, keyedStart(scope, "att-1"))
	if err != nil || !created || next.ID == old.ID {
		t.Fatalf("poll after expiry = %s created=%v err=%v, want a new attempt",
			next.ID, created, err)
	}
	if next.Attempt != 2 || next.Supersedes != old.ID || next.State != StateQueued ||
		next.IncidentID != "att-1" || next.CaseID != old.CaseID {
		t.Fatalf("attempt 2 = attempt %d supersedes %q state %s incident %q case %q",
			next.Attempt, next.Supersedes, next.State, next.IncidentID, next.CaseID)
	}
	expired, err := st.Get(ctx, scope, old.ID)
	if err != nil || expired.State != StateExpired ||
		expired.FailureCode != ReasonQueueExpired || expired.SupersededBy != next.ID {
		t.Fatalf("attempt 1 = %s %q superseded by %q (%v)", expired.State,
			expired.FailureCode, expired.SupersededBy, err)
	}
	events, payloads := eventsOf(t, ctx, st, next)
	if len(events) != 1 || events[0].Type != EventCreated ||
		payloads[0]["attempt"] != float64(2) || payloads[0]["supersedes"] != string(old.ID) {
		t.Fatalf("attempt 2 events = %d, first %s %v", len(events), events[0].Type,
			payloads[0])
	}
	again, created, err := st.Create(ctx, keyedStart(scope, "att-1"))
	if err != nil || created || again.ID != next.ID {
		t.Fatalf("a poll while attempt 2 waits = %s created=%v err=%v", again.ID,
			created, err)
	}
}

// CHECK-33 (state transition): a paused (stopped) investigation stays
// paused past its queue expiry while its trigger keeps polling.
func TestStore_PausedRunIsNeverExpiredByCreate(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, _, err := st.Create(ctx, keyedStart(scope, "att-2"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	paused, err := st.Pause(ctx, scope, inv.ID, inv.Version)
	if err != nil || paused.State != StatePaused {
		t.Fatalf("pause = %s (%v)", paused.State, err)
	}
	setQueueExpiry(t, ctx, pool, paused, -time.Minute)
	for _, req := range []StartRequest{keyedStart(scope, "att-2"), lockStart(scope, "att-2")} {
		got, created, err := st.Create(ctx, req)
		if err != nil || created || got.ID != inv.ID || got.State != StatePaused {
			t.Fatalf("poll of a paused trigger (key %q) = %s %s created=%v err=%v",
				req.IdempotencyKey, got.ID, got.State, created, err)
		}
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StatePaused || got.FailureCode != "" || got.Version != paused.Version {
		t.Fatalf("paused run after polls = %s %q version %d, want paused at %d",
			got.State, got.FailureCode, got.Version, paused.Version)
	}
	if _, err := st.Resume(ctx, scope, inv.ID, got.Version); err != nil {
		t.Fatalf("resume after the polls: %v", err)
	}
}

// Integration: an expiry by Create is fenced and recorded on the chain as
// the system's transition with reason queue_expired, and the chain
// verifies.
func TestStore_QueueExpiryIsFencedAndAudited(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, _, err := st.Create(ctx, keyedStart(scope, "att-3"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setQueueExpiry(t, ctx, pool, inv, -time.Millisecond)
	if _, _, err := st.Create(ctx, keyedStart(scope, "att-3")); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateExpired || got.Fence != inv.Fence+1 ||
		got.Version != inv.Version+1 || got.ConcludedAt.IsZero() {
		t.Fatalf("expired = %s fence %d version %d concluded %v; want fence %d version %d",
			got.State, got.Fence, got.Version, got.ConcludedAt, inv.Fence+1, inv.Version+1)
	}
	events, payloads := eventsOf(t, ctx, st, inv)
	last := len(events) - 1
	if len(events) != 2 || events[last].Type != EventTransition ||
		events[last].Actor != "system" || payloads[last]["to"] != string(StateExpired) ||
		payloads[last]["reason"] != ReasonQueueExpired {
		t.Fatalf("expiry events = %d, last %s by %s %v", len(events), events[last].Type,
			events[last].Actor, payloads[last])
	}
	if err := st.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatalf("chain after expiry: %v", err)
	}
}

// Boundary: an expiry in the future (1 s, the nearest instant a moving
// clock can pin; the predicate is a strict expires_at < now) keeps the
// run; 1 ms past expires it.
func TestStore_QueueExpiryBoundary(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, _, err := st.Create(ctx, keyedStart(scope, "att-4"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setQueueExpiry(t, ctx, pool, inv, 2*time.Second)
	same, created, err := st.Create(ctx, keyedStart(scope, "att-4"))
	if err != nil || created || same.ID != inv.ID || same.State != StateQueued {
		t.Fatalf("poll before expiry = %s %s created=%v err=%v", same.ID, same.State,
			created, err)
	}
	setQueueExpiry(t, ctx, pool, inv, -time.Millisecond)
	next, created, err := st.Create(ctx, keyedStart(scope, "att-4"))
	if err != nil || !created || next.Attempt != 2 {
		t.Fatalf("poll 1 ms past expiry = attempt %d created=%v err=%v", next.Attempt,
			created, err)
	}
}

// Boundary: attempts 1-5 run; after the 5th expires the poll returns it
// (created false) and records attempts_exhausted once, however often the
// trigger polls.
func TestStore_AttemptsStopAtFive(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	cur, _, err := st.Create(ctx, keyedStart(scope, "att-5"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for want := 2; want <= MaxAttempts; want++ {
		setQueueExpiry(t, ctx, pool, cur, -time.Millisecond)
		next, created, err := st.Create(ctx, keyedStart(scope, "att-5"))
		if err != nil || !created || next.Attempt != want || next.Supersedes != cur.ID {
			t.Fatalf("attempt %d = attempt %d supersedes %q created=%v err=%v", want,
				next.Attempt, next.Supersedes, created, err)
		}
		cur = next
	}
	setQueueExpiry(t, ctx, pool, cur, -time.Millisecond)
	for poll := 0; poll < 3; poll++ {
		got, created, err := st.Create(ctx, keyedStart(scope, "att-5"))
		if err != nil || created || got.ID != cur.ID || got.State != StateExpired ||
			got.Attempt != MaxAttempts {
			t.Fatalf("poll %d after the last attempt = %s attempt %d %s created=%v err=%v",
				poll, got.ID, got.Attempt, got.State, created, err)
		}
	}
	if n := countKeyRows(t, ctx, pool, scope, "incident:att-5"); n != MaxAttempts {
		t.Fatalf("rows under the key = %d, want %d", n, MaxAttempts)
	}
	events, payloads := eventsOf(t, ctx, st, cur)
	exhausted := 0
	for i, e := range events {
		if e.Type == EventAttemptsExhausted {
			exhausted++
			if e.Actor != "system" || payloads[i]["attempt"] != float64(MaxAttempts) {
				t.Fatalf("attempts_exhausted = by %s %v", e.Actor, payloads[i])
			}
		}
	}
	if exhausted != 1 {
		t.Fatalf("attempts_exhausted events = %d, want 1", exhausted)
	}
	if err := st.VerifyEvents(ctx, scope, cur.ID); err != nil {
		t.Fatalf("chain of the last attempt: %v", err)
	}
}

// Concurrent access: racing polls after an expiry create exactly one new
// attempt and all see it.
func TestStore_RacingPollsAfterExpiryCreateOneAttempt(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	old, _, err := st.Create(ctx, keyedStart(scope, "att-6"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setQueueExpiry(t, ctx, pool, old, -time.Millisecond)
	const n = 8
	ids := make([]UUID, n)
	createdCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv, created, err := st.Create(ctx, keyedStart(scope, "att-6"))
			if err != nil {
				t.Errorf("poll %d: %v", i, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[i] = inv.ID
			if created {
				createdCount++
			}
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] || id == old.ID {
			t.Fatalf("racing polls returned %v (old %s)", ids, old.ID)
		}
	}
	if createdCount != 1 || countKeyRows(t, ctx, pool, scope, "incident:att-6") != 2 {
		t.Fatalf("created = %d, rows = %d; want one new attempt", createdCount,
			countKeyRows(t, ctx, pool, scope, "incident:att-6"))
	}
	if _, payloads := eventsOf(t, ctx, st, old); len(payloads) != 2 {
		t.Fatalf("attempt 1 has %d events, want created and one expiry", len(payloads))
	}
}

// Invalid input for a retry: an operator's key keeps HTTP idempotency
// (the same expired row), and terminal rows that are not a retryable
// expiry are returned unchanged to the trigger too.
func TestStore_OnlySystemRetriesOfRetryableExpiries(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	op := lockStart(scope, "att-7")
	op.IdempotencyKey, op.Actor = "operator-request-sre01", "user:7"
	inv, _, err := st.Create(ctx, op)
	if err != nil {
		t.Fatalf("operator create: %v", err)
	}
	setQueueExpiry(t, ctx, pool, inv, -time.Millisecond)
	for i := 0; i < 2; i++ {
		got, created, err := st.Create(ctx, op)
		if err != nil || created || got.ID != inv.ID || got.Attempt != 1 {
			t.Fatalf("operator repeat %d = %s attempt %d created=%v err=%v", i, got.ID,
				got.Attempt, created, err)
		}
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateExpired || got.FailureCode != ReasonQueueExpired {
		t.Fatalf("operator row = %s %q, want expired queue_expired", got.State,
			got.FailureCode)
	}
	concluded, _, _ := st.Create(ctx, keyedStart(scope, "att-8"))
	driveToConcluded(t, ctx, st, concluded)
	cancelled, _, _ := st.Create(ctx, keyedStart(scope, "att-9"))
	if _, err := st.Stop(ctx, scope, cancelled.ID, cancelled.Version); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for _, subject := range []string{"att-8", "att-9"} {
		got, created, err := st.Create(ctx, keyedStart(scope, subject))
		if err != nil || created || got.Attempt != 1 || !got.State.Terminal() {
			t.Fatalf("poll of finished %s = %s attempt %d created=%v err=%v", subject,
				got.State, got.Attempt, created, err)
		}
	}
}

// CHECK-32 (integration): every queue expiry, by a poll or at claim, is
// one system transition event with reason queue_expired.
func TestStore_EveryQueueExpiryHasItsEvent(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	byPoll, _, _ := st.Create(ctx, keyedStart(scope, "att-10"))
	setQueueExpiry(t, ctx, pool, byPoll, -time.Millisecond)
	if _, _, err := st.Create(ctx, keyedStart(scope, "att-10")); err != nil {
		t.Fatalf("poll: %v", err)
	}
	byClaim, _, _ := st.Create(ctx, lockStart(scope, "att-11"))
	setQueueExpiry(t, ctx, pool, byClaim, -time.Millisecond)
	if _, err := st.Claim(ctx, scope, byClaim.ID, NewUUID()); !errors.Is(err, ErrTerminal) {
		t.Fatalf("claim of an expired run = %v, want ErrTerminal", err)
	}
	var rows, events int
	if err := pool.QueryRow(ctx, `SELECT
		    (SELECT count(*) FROM sage.sre_investigations
		      WHERE deployment_id = $1 AND database_id = $2
		        AND state = 'expired' AND failure_code = 'queue_expired'),
		    (SELECT count(*) FROM sage.sre_events
		      WHERE deployment_id = $1 AND database_id = $2 AND event_type = 'transition'
		        AND actor = 'system' AND payload->>'reason' = 'queue_expired')`,
		string(scope.DeploymentID), string(scope.DatabaseID)).Scan(&rows, &events); err != nil {
		t.Fatalf("count expiries: %v", err)
	}
	if rows != 2 || events != rows {
		t.Fatalf("queue_expired rows = %d, events = %d; want 2 and 2", rows, events)
	}
}

// exhaust runs a key through MaxAttempts attempts and returns the last
// one, past its queue expiry and still queued.
func exhaust(t *testing.T, ctx context.Context, st *PostgresStore, pool *pgxpool.Pool,
	scope Scope, subject string) Investigation {
	t.Helper()
	cur, _, err := st.Create(ctx, keyedStart(scope, subject))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for cur.Attempt < MaxAttempts {
		setQueueExpiry(t, ctx, pool, cur, -time.Millisecond)
		if cur, _, err = st.Create(ctx, keyedStart(scope, subject)); err != nil {
			t.Fatalf("attempt after %d: %v", cur.Attempt, err)
		}
	}
	setQueueExpiry(t, ctx, pool, cur, -time.Millisecond)
	return cur
}

// holdRow locks an investigation row in its own transaction until the
// returned release runs, so racing polls queue on the row lock.
func holdRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	inv Investigation) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3 FOR UPDATE`,
		string(inv.Scope.DeploymentID), string(inv.Scope.DatabaseID),
		string(inv.ID)); err != nil {
		t.Fatalf("lock %s: %v", inv.ID, err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("release %s: %v", inv.ID, err)
		}
	}
}

// waitForLockWaiters waits until n sessions wait on a lock.
func waitForLockWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).
			Scan(&waiting); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d sessions waited on the row lock", n)
}

// Concurrent access: polls that queue on the last attempt's row lock
// after it expired at claim (no poll saw it yet) record
// attempts_exhausted exactly once (each re-reads under the lock) and
// start nothing.
func TestStore_RacingPollsAfterTheLastAttemptRecordExhaustedOnce(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	last := exhaust(t, ctx, st, pool, scope, "att-12")
	if _, err := st.Claim(ctx, scope, last.ID, NewUUID()); !errors.Is(err, ErrTerminal) {
		t.Fatalf("claim of the expired last attempt = %v, want ErrTerminal", err)
	}
	const n = 2 // with the holder and the waiter count, within the 4-connection pool floor
	release := holdRow(t, ctx, pool, last)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, created, err := st.Create(ctx, keyedStart(scope, "att-12"))
			if err != nil || created || got.ID != last.ID {
				t.Errorf("poll %d = %s created=%v err=%v, want the last attempt %s", i,
					got.ID, created, err, last.ID)
			}
		}(i)
	}
	waitForLockWaiters(t, ctx, pool, n)
	release()
	wg.Wait()
	events, _ := eventsOf(t, ctx, st, last)
	exhausted := 0
	for _, e := range events {
		if e.Type == EventAttemptsExhausted {
			exhausted++
		}
	}
	if exhausted != 1 || countKeyRows(t, ctx, pool, scope, "incident:att-12") != MaxAttempts {
		t.Fatalf("attempts_exhausted events = %d (want 1), rows = %d (want %d)", exhausted,
			countKeyRows(t, ctx, pool, scope, "incident:att-12"), MaxAttempts)
	}
	if err := st.VerifyEvents(ctx, scope, last.ID); err != nil {
		t.Fatalf("chain after racing polls: %v", err)
	}
}

// Invalid input: an operator's request cannot carry the trigger loop's
// actor, which alone earns automatic re-runs.
func TestService_StartRefusesTheTriggerActor(t *testing.T) {
	svc, ctx := opService(t)
	tr := opTrigger()
	tr.Actor = triggerActor
	inv, created, err := svc.Start(ctx, tr)
	if !errors.Is(err, ErrInvalidRequest) || created || inv.ID != "" {
		t.Fatalf("start as %q = %s created=%v err=%v, want ErrInvalidRequest", triggerActor,
			inv.ID, created, err)
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("refusal %q does not say the actor is reserved", err)
	}
}
