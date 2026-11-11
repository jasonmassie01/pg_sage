package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-08: the conclusion notifier hands each incident's newest settled
// diagnosis to the notify stack at most once, from any number of
// sidecars, and never for failed, expired or cancelled runs.

const plantedApp = "billing password=planted-app-secret"

type recordingSink struct {
	mu  sync.Mutex
	got []ConclusionSignal
	err error
}

func (s *recordingSink) DeliverConclusion(_ context.Context, sig ConclusionSignal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sig)
	return s.err
}

func (s *recordingSink) signals() []ConclusionSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ConclusionSignal(nil), s.got...)
}

// flakyIncidents fails the incident read while broken is set.
type flakyIncidents struct {
	base   ConclusionIncidents
	broken bool
}

func (f *flakyIncidents) TrackedIncident(ctx context.Context, id string) (TrackedIncident,
	bool, error) {
	if f.broken {
		return TrackedIncident{}, false, errors.New("incident read: connection refused")
	}
	return f.base.TrackedIncident(ctx, id)
}

func followUpCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	key string, sink ConclusionSink, incidents ConclusionIncidents) (*Coordinator, Scope) {
	t.Helper()
	cfg := DefaultCoordinatorConfig(key)
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: newScriptedRunner(),
		Config: cfg, FollowUps: sink, TrackedIncidents: incidents})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	scope, err := c.Bind(ctx)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c, scope
}

func insertFollowUpIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db,
	severity string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, signal_ids, source, database_name)
		VALUES ($1, 'lock contention', ARRAY['lock_contention'], 'deterministic', $2)
		RETURNING id::text`, severity, db).Scan(&id); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	t.Cleanup(func() { // other packages' trigger tests read every open incident
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.incidents
			WHERE id = $1::uuid`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.sre_incident_followup
			WHERE incident_id = $1`, id)
	})
	return id
}

// runOfIncident creates a lock investigation of incident and drives it to state.
func runOfIncident(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
	incident string, state State, reason string) Investigation {
	t.Helper()
	req := lockStart(scope, "run-"+string(NewUUID()))
	req.IncidentID, req.Actor = incident, "trigger:rca"
	inv, _, err := st.Create(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if state == StateQueued {
		return inv
	}
	if state == StateCancelled {
		out, err := st.OperatorTransition(ctx, scope, inv.ID, inv.Version, StateCancelled, "op")
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		return out
	}
	lease, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, lease, step("step-1", StateEvaluating,
		probeResult(probes.LockGraph, probes.Row{"pid": int64(7),
			"application_name": plantedApp}),
		probeResult(probes.PreparedXacts))); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev, err := st.Evidence(ctx, scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	out, err := finish(ctx, st, lease, ev, state, reason)
	if err != nil {
		t.Fatalf("finish %s: %v", state, err)
	}
	return out
}

func finish(ctx context.Context, st *PostgresStore, lease Lease, ev []Evidence,
	state State, reason string) (Investigation, error) {
	switch state {
	case StateConcluded:
		c := concluded(ev)
		c.Summary.Reason = reason
		return st.Conclude(ctx, lease, c)
	case StateInconclusive:
		return st.Conclude(ctx, lease, Conclusion{State: StateInconclusive,
			Summary: Summary{Family: "lock_blocking", GraphVersion: "causal-v2",
				Reason: reason}, Hypotheses: []HypothesisRecord{ruledOutHypothesis(ev)}})
	case StateFailed:
		return st.Conclude(ctx, lease, Conclusion{State: StateFailed,
			FailureCode: "probe_failed", Summary: Summary{Reason: reason}})
	case StateExpired:
		return st.ExpireStale(ctx, lease, Summary{Reason: reason})
	}
	return Investigation{}, fmt.Errorf("no fixture for state %s", state)
}

// ageConclusion moves an investigation's conclusion d into the past (setting one on
// runs that never concluded, so only the state can exclude them).
func ageConclusion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id UUID,
	d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET concluded_at = clock_timestamp() - make_interval(secs => $2)
		WHERE id = $1::uuid`, string(id), d.Seconds()); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
}

type followUpClaim struct {
	investigation UUID
	delivered     bool
}

func claimOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, incident string) (followUpClaim,
	bool) {
	t.Helper()
	var r followUpClaim
	var inv string
	err := pool.QueryRow(ctx, `SELECT investigation_id::text, delivered_at IS NOT NULL
		FROM sage.sre_incident_followup WHERE incident_id = $1`, incident).
		Scan(&inv, &r.delivered)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return followUpClaim{}, false
		}
		t.Fatalf("read claim: %v", err)
	}
	r.investigation = UUID(inv)
	return r, true
}

func TestConclusionNotifier_SendsTheSettledConclusionOnce(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root blocker pid 7 is idle")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
	c.notifyConclusions(ctx, scope)
	c.notifyConclusions(ctx, scope)
	got := sink.signals()
	if len(got) != 1 {
		t.Fatalf("%d follow-ups, want 1: %+v", len(got), got)
	}
	s := got[0]
	if s.InvestigationID != a.ID || s.IncidentID != inc || s.CaseID != a.CaseID ||
		s.Database != "orders" || s.Severity != "critical" || s.Family != "lock_blocking" ||
		s.State != StateConcluded || s.RootLabel != "idle-in-transaction holder" ||
		s.Reason != "root blocker pid 7 is idle" || s.ConcludedAt.IsZero() {
		t.Fatalf("signal = %+v", s)
	}
	claim, ok := claimOf(t, ctx, pool, inc)
	if !ok || claim.investigation != a.ID || !claim.delivered {
		t.Fatalf("claim = %+v (%v), want delivered for %s", claim, ok, a.ID)
	}
	// A later re-run of the same incident concludes: no second follow-up.
	b := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "re-run diagnosis")
	ageConclusion(t, ctx, pool, b.ID, 2*time.Minute)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 1 {
		t.Fatalf("re-run conclusion sent again: %d follow-ups", n)
	}
}

func TestConclusionNotifier_InconclusiveIsDelivered(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "warning")
	a := runOfIncident(t, ctx, st, scope, inc, StateInconclusive, "lock graph empty when probed")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
	c.notifyConclusions(ctx, scope)
	got := sink.signals()
	if len(got) != 1 || got[0].State != StateInconclusive || got[0].RootLabel != "" ||
		got[0].Severity != "warning" || got[0].Reason != "lock graph empty when probed" {
		t.Fatalf("inconclusive follow-ups = %+v", got)
	}
}

func TestConclusionNotifier_WaitsForTheConclusionToSettle(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "fresh")
	ageConclusion(t, ctx, pool, a.ID, 50*time.Second)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("a 50 s old conclusion was sent: %d", n)
	}
	if _, ok := claimOf(t, ctx, pool, inc); ok {
		t.Fatal("an unsettled conclusion was claimed")
	}
	ageConclusion(t, ctx, pool, a.ID, 61*time.Second)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 1 {
		t.Fatalf("a 61 s old conclusion: %d follow-ups, want 1", n)
	}
}

func TestConclusionNotifier_IgnoresConclusionsOlderThanTheWindow(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "old news")
	ageConclusion(t, ctx, pool, a.ID, 61*time.Minute)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("a 61 minute old conclusion was sent: %d", n)
	}
	ageConclusion(t, ctx, pool, a.ID, 59*time.Minute)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 1 {
		t.Fatalf("a 59 minute old conclusion: %d follow-ups, want 1", n)
	}
}

func TestConclusionNotifier_TwoConclusionsSendOnlyTheNewer(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	older := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "first diagnosis")
	newer := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "second diagnosis")
	ageConclusion(t, ctx, pool, older.ID, 90*time.Second)
	ageConclusion(t, ctx, pool, newer.ID, 30*time.Second)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("the older conclusion went out while the newer settles: %d", n)
	}
	ageConclusion(t, ctx, pool, newer.ID, 70*time.Second)
	c.notifyConclusions(ctx, scope)
	got := sink.signals()
	if len(got) != 1 || got[0].InvestigationID != newer.ID ||
		got[0].Reason != "second diagnosis" {
		t.Fatalf("follow-ups = %+v, want only %s", got, newer.ID)
	}
}

func TestConclusionNotifier_WaitsForALiveRunOfTheIncident(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	done := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "first")
	ageConclusion(t, ctx, pool, done.ID, 2*time.Minute)
	live := runOfIncident(t, ctx, st, scope, inc, StateQueued, "")
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("sent while a run of the incident is queued: %d", n)
	}
	if _, err := st.OperatorTransition(ctx, scope, live.ID, live.Version, StateCancelled,
		"op"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c.notifyConclusions(ctx, scope)
	got := sink.signals()
	if len(got) != 1 || got[0].InvestigationID != done.ID {
		t.Fatalf("after the live run ended: %+v", got)
	}
}

// CHECK-35: failed, expired and cancelled runs get no follow-up.
func TestConclusionNotifier_NoFollowUpForFailedExpiredOrCancelled(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	var incidents []string
	for _, state := range []State{StateFailed, StateExpired, StateCancelled} {
		inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
		incidents = append(incidents, inc)
		inv := runOfIncident(t, ctx, st, scope, inc, state, "did not finish")
		if inv.State != state {
			t.Fatalf("fixture state = %s, want %s", inv.State, state)
		}
		ageConclusion(t, ctx, pool, inv.ID, 2*time.Minute)
	}
	c.notifyConclusions(ctx, scope)
	if got := sink.signals(); len(got) != 0 {
		t.Fatalf("CHECK-35: %d deliveries for unfinished runs: %+v", len(got), got)
	}
	for _, inc := range incidents {
		if _, ok := claimOf(t, ctx, pool, inc); ok {
			t.Fatalf("incident %s was claimed", inc)
		}
	}
}

func TestConclusionNotifier_NoIncidentOrUntrackedIncidentSendsNothing(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	operator := runOfIncident(t, ctx, st, scope, "", StateConcluded, "operator asked")
	gone := string(NewUUID()) // never recorded (or purged by retention)
	elsewhere := insertFollowUpIncident(t, ctx, pool, "payments", "critical")
	runs := []Investigation{operator,
		runOfIncident(t, ctx, st, scope, gone, StateConcluded, "gone"),
		runOfIncident(t, ctx, st, scope, elsewhere, StateConcluded, "other database")}
	for _, r := range runs {
		ageConclusion(t, ctx, pool, r.ID, 2*time.Minute)
	}
	c.notifyConclusions(ctx, scope)
	if got := sink.signals(); len(got) != 0 {
		t.Fatalf("sent %d follow-ups without a tracked incident: %+v", len(got), got)
	}
	for _, inc := range []string{gone, elsewhere} {
		if _, ok := claimOf(t, ctx, pool, inc); ok {
			t.Fatalf("untracked incident %s was claimed", inc)
		}
	}
}

func TestConclusionNotifier_DeliveryFailureKeepsTheClaim(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{err: errors.New("slack returned status 503")}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root found")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
	for i := 0; i < 3; i++ {
		c.notifyConclusions(ctx, scope)
	}
	if n := len(sink.signals()); n != 1 {
		t.Fatalf("a failed delivery was retried: %d attempts, want 1", n)
	}
	claim, ok := claimOf(t, ctx, pool, inc)
	if !ok || claim.delivered || claim.investigation != a.ID {
		t.Fatalf("claim after failure = %+v (%v), want undelivered claim of %s",
			claim, ok, a.ID)
	}
}

func TestConclusionNotifier_IncidentReadErrorDefersWithoutClaim(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	incidents := &flakyIncidents{base: NewPGTriggerSource(pool, "orders"), broken: true}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink, incidents)
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root found")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
	c.notifyConclusions(ctx, scope)
	if _, ok := claimOf(t, ctx, pool, inc); ok || len(sink.signals()) != 0 {
		t.Fatal("an unreadable incident was claimed or sent")
	}
	incidents.broken = false
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 1 {
		t.Fatalf("after the incident read recovered: %d follow-ups, want 1", n)
	}
}
