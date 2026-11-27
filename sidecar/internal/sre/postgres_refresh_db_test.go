package sre

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// playbooks-06: an operator re-reads a finished investigation's evidence.
// The refresh is one step, one evidence row (source operator_refresh) and
// one evidence_refreshed event, under the investigation lock, so the
// event chain stays whole and the run's state does not move.

// finishedRun is a concluded investigation with two probe evidence rows.
func finishedRun(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	subject string) (Investigation, []Evidence) {
	t.Helper()
	lease, ev := evaluating(t, ctx, st, scope, subject)
	inv, err := st.Conclude(ctx, lease, concluded(ev))
	if err != nil {
		t.Fatalf("conclude: %v", err)
	}
	return inv, ev
}

// lockOf is the run's lock_graph evidence (evaluating stores two probes;
// their order within the step is by observation time, not by argument).
func lockOf(ev []Evidence) UUID {
	for _, e := range ev {
		if e.ProbeID == string(probes.LockGraph) {
			return e.ID
		}
	}
	return ""
}

// rewriteEvidence updates sre_evidence behind its append-only trigger, as
// a superuser could (a tamper, or moving a row's clock for a boundary).
func rewriteEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string,
	args ...any) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("rewrite: %v (%d rows)", err, tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func lockRefresh(source UUID, pid int64) RefreshRequest {
	return RefreshRequest{SourceEvidenceID: source,
		Result: probeResult(probes.LockGraph, probes.Row{"pid": pid})}
}

func TestRefresh_AppendsOneStepEvidenceAndEvent(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, ev := finishedRun(t, ctx, st, scope, "pid 61")
	got, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", lockRefresh(lockOf(ev), 9))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got.ID == "" || got.ID == ev[0].ID || got.ProbeID != string(probes.LockGraph) ||
		!strings.HasPrefix(got.StepKey, RefreshKeyPrefix) || !got.VerifyHash() ||
		got.CapabilityState != "available" || got.ObservedAt.IsZero() {
		t.Fatalf("refresh evidence = %+v", got)
	}
	var rows []map[string]any
	if err := json.Unmarshal(got.Payload, &struct {
		Rows *[]map[string]any `json:"rows"`
	}{&rows}); err != nil || len(rows) != 1 || rows[0]["pid"] != float64(9) {
		t.Fatalf("refresh payload = %s (%v)", got.Payload, err)
	}
	var kind, next string
	var probeCount int
	var fence int64
	if err := pool.QueryRow(ctx, `SELECT e.source_kind, st.next_state, st.probe_count,
		    st.fence_token
		FROM sage.sre_evidence e JOIN sage.sre_steps st ON st.id = e.step_id
		 AND st.investigation_id = e.investigation_id
		WHERE e.id = $1`, string(got.ID)).Scan(&kind, &next, &probeCount, &fence); err != nil {
		t.Fatalf("refresh rows: %v", err)
	}
	if kind != SourceOperatorRefresh || next != string(StateConcluded) || probeCount != 1 ||
		fence != inv.Fence {
		t.Fatalf("refresh step = %s/%s/%d/fence %d, want operator_refresh/concluded/1/%d",
			kind, next, probeCount, fence, inv.Fence)
	}
	checkRefreshEvent(t, ctx, st, scope, inv.ID, got, lockOf(ev))
	if err := st.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatalf("verify after refresh: %v", err)
	}
	after, err := st.Get(ctx, scope, inv.ID)
	if err != nil || after.State != StateConcluded || after.Version != inv.Version ||
		after.ProbeCount != inv.ProbeCount {
		t.Fatalf("investigation moved: %+v (%v), before %+v", after, err, inv)
	}
	all, err := st.Evidence(ctx, scope, inv.ID)
	if err != nil || len(all) != 3 || all[2].ID != got.ID || lockOf(ev) == "" {
		t.Fatalf("evidence after refresh = %d rows (%v), want the refresh last", len(all), err)
	}
}

// checkRefreshEvent requires the chain's last event to attest to got.
func checkRefreshEvent(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	id UUID, got Evidence, source UUID) {
	t.Helper()
	events, err := st.Events(ctx, scope, id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	last := events[len(events)-1]
	var payload map[string]any
	_ = json.Unmarshal(last.Payload, &payload)
	if last.Type != EventEvidenceRefreshed || last.Actor != "user:1" ||
		payload["evidence_id"] != string(got.ID) ||
		payload["source_evidence_id"] != string(source) ||
		payload["sha256"] != hex.EncodeToString(got.SHA256) ||
		payload["step_key"] != got.StepKey {
		t.Fatalf("refresh event = %s %s %v", last.Type, last.Actor, payload)
	}
}

func TestRefresh_InconclusiveRunIsAllowedAndKeepsItsState(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 62")
	c := concluded(ev)
	c.State, c.Summary.Conclusive, c.Summary.Root = StateInconclusive, false, ""
	c.Hypotheses = []HypothesisRecord{ruledOutHypothesis(ev)}
	inv, err := st.Conclude(ctx, lease, c)
	if err != nil {
		t.Fatalf("inconclusive: %v", err)
	}
	got, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", lockRefresh(lockOf(ev), 3))
	if err != nil {
		t.Fatalf("refresh of an inconclusive run: %v", err)
	}
	after, _ := st.Get(ctx, scope, inv.ID)
	if after.State != StateInconclusive || got.ID == "" {
		t.Fatalf("state after refresh = %s, evidence %q", after.State, got.ID)
	}
}

func TestRefresh_RefusesRunsThatAreNotFinishedOrArePurged(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	lease, ev := evaluating(t, ctx, st, scope, "pid 63")
	if _, err := st.AppendRefreshEvidence(ctx, scope, lease.InvestigationID, "user:1",
		lockRefresh(lockOf(ev), 1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("refresh of an evaluating run = %v, want ErrInvalidRequest", err)
	}
	inv, _ := st.Get(ctx, scope, lease.InvestigationID)
	if _, err := st.Stop(ctx, scope, inv.ID, inv.Version); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1",
		lockRefresh(lockOf(ev), 1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("refresh of a cancelled run = %v, want ErrInvalidRequest", err)
	}
	failedLease, failedEv := evaluating(t, ctx, st, scope, "pid 64")
	if _, err := st.Conclude(ctx, failedLease, Conclusion{State: StateFailed,
		FailureCode: "probe_error"}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, err := st.AppendRefreshEvidence(ctx, scope, failedLease.InvestigationID, "user:1",
		lockRefresh(lockOf(failedEv), 1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("refresh of a failed run = %v, want ErrInvalidRequest", err)
	}
	done, doneEv := finishedRun(t, ctx, st, scope, "pid 65")
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET evidence_purged_at = clock_timestamp() WHERE id = $1`, string(done.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendRefreshEvidence(ctx, scope, done.ID, "user:1",
		lockRefresh(lockOf(doneEv), 1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("refresh of purged evidence = %v, want ErrInvalidRequest", err)
	}
	for _, id := range []UUID{inv.ID, failedLease.InvestigationID, done.ID} {
		events, _ := st.Events(ctx, scope, id)
		if events[len(events)-1].Type == EventEvidenceRefreshed {
			t.Fatalf("a refused refresh appended an event to %s", id)
		}
	}
}

func TestRefresh_InvalidInputIsRefused(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, ev := finishedRun(t, ctx, st, scope, "pid 66")
	_, foreign := finishedRun(t, ctx, st, testScope(t, ctx, st), "pid 66")
	wrongProbe := lockRefresh(lockOf(ev), 1)
	wrongProbe.Result.ProbeID = probes.PreparedXacts
	negative := lockRefresh(lockOf(ev), 1)
	negative.MinInterval = -time.Second
	for name, tc := range map[string]struct {
		actor string
		req   RefreshRequest
	}{
		"blank actor":        {"", lockRefresh(lockOf(ev), 1)},
		"long actor":         {strings.Repeat("a", 129), lockRefresh(lockOf(ev), 1)},
		"malformed source":   {"user:1", lockRefresh("not-a-uuid", 1)},
		"unknown source":     {"user:1", lockRefresh(NewUUID(), 1)},
		"foreign source":     {"user:1", lockRefresh(foreign[0].ID, 1)},
		"other probe":        {"user:1", wrongProbe},
		"negative interval":  {"user:1", negative},
		"empty probe result": {"user:1", RefreshRequest{SourceEvidenceID: ev[0].ID}},
		"zero source id":     {"user:1", lockRefresh("", 1)},
		"sql-looking actor": {"user:1'; DROP TABLE sage.sre_events; --",
			lockRefresh(NewUUID(), 1)},
		"all-zero source": {"user:1", lockRefresh(UUID(strings.Repeat("0", 36)), 1)},
	} {
		if _, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, tc.actor,
			tc.req); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want ErrInvalidRequest or ErrNotFound", name, err)
		}
	}
	all, _ := st.Evidence(ctx, scope, inv.ID)
	if len(all) != 2 {
		t.Fatalf("refused refreshes wrote evidence: %d rows", len(all))
	}
}

// Boundary: one refresh per investigation per MinInterval, on the
// database clock. 59 s after the last refresh is refused, 60 s allowed.
func TestRefresh_RateLimitBoundary(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, ev := finishedRun(t, ctx, st, scope, "pid 67")
	req := lockRefresh(lockOf(ev), 1)
	req.MinInterval = time.Minute
	first, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", req)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1",
		req); !errors.Is(err, ErrRefreshTooSoon) {
		t.Fatalf("immediate second refresh = %v, want ErrRefreshTooSoon", err)
	}
	age := func(secs int) {
		t.Helper()
		rewriteEvidence(t, ctx, pool, `UPDATE sage.sre_evidence
			SET collected_at = clock_timestamp() - make_interval(secs => $2)
			WHERE id = $1`, string(first.ID), secs)
	}
	age(59)
	if _, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1",
		req); !errors.Is(err, ErrRefreshTooSoon) {
		t.Fatalf("refresh 59 s later = %v, want ErrRefreshTooSoon", err)
	}
	age(60)
	if _, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", req); err != nil {
		t.Fatalf("refresh 60 s later: %v", err)
	}
	if err := st.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Integrity: a rewritten refresh payload fails its hash; a rewritten
// hash, or a refresh row inserted outside AppendRefreshEvidence, fails
// the event chain because no evidence_refreshed event attests to it.
func TestRefresh_TamperingIsDetected(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, ev := finishedRun(t, ctx, st, scope, "pid 68")
	got, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", lockRefresh(lockOf(ev), 4))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rewriteEvidence(t, ctx, pool, `UPDATE sage.sre_evidence
		SET payload = jsonb_set(payload, '{rows,0,pid}', '5') WHERE id = $1`, string(got.ID))
	tampered, err := st.EvidenceByID(ctx, scope, inv.ID, got.ID)
	if err != nil || tampered.VerifyHash() {
		t.Fatalf("tampered refresh row verifies (%v)", err)
	}
	rewriteEvidence(t, ctx, pool, `UPDATE sage.sre_evidence
		SET sha256 = sha256(convert_to(payload::text, 'UTF8')) WHERE id = $1`, string(got.ID))
	if err := st.VerifyEvents(ctx, scope, inv.ID); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("rehashed refresh row = %v, want ErrChainBroken", err)
	}
	clean, cleanEv := finishedRun(t, ctx, st, scope, "pid 69")
	if _, err := st.AppendRefreshEvidence(ctx, scope, clean.ID, "user:1",
		lockRefresh(lockOf(cleanEv), 4)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_evidence
		(deployment_id, database_id, investigation_id, id, step_id, source_kind, probe_id,
		 probe_version, observed_at, capability_state, payload_version, classification,
		 payload, sha256)
		SELECT e.deployment_id, e.database_id, e.investigation_id, gen_random_uuid(),
		       e.step_id, 'operator_refresh', e.probe_id, e.probe_version,
		       clock_timestamp(), 'available', 1, 'redacted', e.payload, e.sha256
		FROM sage.sre_evidence e
		WHERE e.investigation_id = $1 AND e.source_kind = 'operator_refresh'`,
		string(clean.ID)); err != nil {
		t.Fatal(err)
	}
	if err := st.VerifyEvents(ctx, scope, clean.ID); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("out-of-band refresh insert = %v, want ErrChainBroken", err)
	}
}

// Concurrency: refreshes serialize on the investigation lock; two at
// once both land, each with its own step, row and event.
func TestRefresh_ConcurrentRefreshesKeepTheChain(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, ev := finishedRun(t, ctx, st, scope, "pid 70")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.AppendRefreshEvidence(context.WithoutCancel(ctx), scope, inv.ID,
				"user:1", lockRefresh(lockOf(ev), int64(i)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	all, _ := st.Evidence(ctx, scope, inv.ID)
	events, _ := st.Events(ctx, scope, inv.ID)
	refreshed := 0
	for _, e := range events {
		if e.Type == EventEvidenceRefreshed {
			refreshed++
		}
	}
	if len(all) != 4 || refreshed != 2 || all[2].StepKey == all[3].StepKey {
		t.Fatalf("after two refreshes: %d evidence rows, %d events", len(all), refreshed)
	}
	if err := st.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// RefreshAllowed is the lock-free precheck callers run before the refresh
// probe; a refused refresh is a rate limit, never a metadata outage, and
// the limit is per investigation.
func TestRefresh_AllowedPrecheckIsTheSameLimit(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, ev := finishedRun(t, ctx, st, scope, "pid 71")
	other, _ := finishedRun(t, ctx, st, scope, "pid 72")
	if err := st.RefreshAllowed(ctx, scope, inv.ID, time.Minute); err != nil {
		t.Fatalf("never refreshed = %v, want allowed", err)
	}
	req := lockRefresh(lockOf(ev), 1)
	req.MinInterval = time.Minute
	if _, err := st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", req); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	err := st.RefreshAllowed(ctx, scope, inv.ID, time.Minute)
	if !errors.Is(err, ErrRefreshTooSoon) || errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("precheck after a refresh = %v, want only ErrRefreshTooSoon", err)
	}
	_, err = st.AppendRefreshEvidence(ctx, scope, inv.ID, "user:1", req)
	if !errors.Is(err, ErrRefreshTooSoon) || errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("locked check = %v, want only ErrRefreshTooSoon", err)
	}
	if err := st.RefreshAllowed(ctx, scope, other.ID, time.Minute); err != nil {
		t.Fatalf("another investigation = %v, want allowed", err)
	}
	if err := st.RefreshAllowed(ctx, scope, inv.ID, 0); err != nil {
		t.Fatalf("zero interval = %v, want no limit", err)
	}
	for name, got := range map[string]error{
		"negative interval": st.RefreshAllowed(ctx, scope, inv.ID, -time.Second),
		"malformed id":      st.RefreshAllowed(ctx, scope, "x", time.Minute),
		"empty scope":       st.RefreshAllowed(ctx, Scope{}, inv.ID, time.Minute),
	} {
		if !errors.Is(got, ErrInvalidRequest) {
			t.Errorf("%s = %v, want ErrInvalidRequest", name, got)
		}
	}
}
