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

func insertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db,
	severity string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, signal_ids, source, database_name)
		VALUES ($1, 'lock contention', ARRAY['lock_contention'], 'deterministic', $2)
		RETURNING id::text`, severity, db).Scan(&id); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	return id
}

// runFor creates a lock investigation of incident and drives it to state.
func runFor(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope,
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

// age moves an investigation's conclusion d into the past (setting one on
// runs that never concluded, so only the state can exclude them).
func age(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id UUID, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET concluded_at = clock_timestamp() - make_interval(secs => $2)
		WHERE id = $1::uuid`, string(id), d.Seconds()); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
}

type claimRow struct {
	investigation UUID
	delivered     bool
}

func claimOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, incident string) (claimRow,
	bool) {
	t.Helper()
	var r claimRow
	var inv string
	err := pool.QueryRow(ctx, `SELECT investigation_id::text, delivered_at IS NOT NULL
		FROM sage.sre_incident_followup WHERE incident_id = $1`, incident).
		Scan(&inv, &r.delivered)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return claimRow{}, false
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "root blocker pid 7 is idle")
	age(t, ctx, pool, a.ID, 2*time.Minute)
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
	b := runFor(t, ctx, st, scope, inc, StateConcluded, "re-run diagnosis")
	age(t, ctx, pool, b.ID, 2*time.Minute)
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
	inc := insertIncident(t, ctx, pool, "orders", "warning")
	a := runFor(t, ctx, st, scope, inc, StateInconclusive, "lock graph empty when probed")
	age(t, ctx, pool, a.ID, 2*time.Minute)
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "fresh")
	age(t, ctx, pool, a.ID, 50*time.Second)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("a 50 s old conclusion was sent: %d", n)
	}
	if _, ok := claimOf(t, ctx, pool, inc); ok {
		t.Fatal("an unsettled conclusion was claimed")
	}
	age(t, ctx, pool, a.ID, 61*time.Second)
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "old news")
	age(t, ctx, pool, a.ID, 61*time.Minute)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("a 61 minute old conclusion was sent: %d", n)
	}
	age(t, ctx, pool, a.ID, 59*time.Minute)
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	older := runFor(t, ctx, st, scope, inc, StateConcluded, "first diagnosis")
	newer := runFor(t, ctx, st, scope, inc, StateConcluded, "second diagnosis")
	age(t, ctx, pool, older.ID, 90*time.Second)
	age(t, ctx, pool, newer.ID, 30*time.Second)
	c.notifyConclusions(ctx, scope)
	if n := len(sink.signals()); n != 0 {
		t.Fatalf("the older conclusion went out while the newer settles: %d", n)
	}
	age(t, ctx, pool, newer.ID, 70*time.Second)
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	done := runFor(t, ctx, st, scope, inc, StateConcluded, "first")
	age(t, ctx, pool, done.ID, 2*time.Minute)
	live := runFor(t, ctx, st, scope, inc, StateQueued, "")
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
		inc := insertIncident(t, ctx, pool, "orders", "critical")
		incidents = append(incidents, inc)
		inv := runFor(t, ctx, st, scope, inc, state, "did not finish")
		if inv.State != state {
			t.Fatalf("fixture state = %s, want %s", inv.State, state)
		}
		age(t, ctx, pool, inv.ID, 2*time.Minute)
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
	operator := runFor(t, ctx, st, scope, "", StateConcluded, "operator asked")
	gone := string(NewUUID()) // never recorded (or purged by retention)
	elsewhere := insertIncident(t, ctx, pool, "payments", "critical")
	runs := []Investigation{operator,
		runFor(t, ctx, st, scope, gone, StateConcluded, "gone"),
		runFor(t, ctx, st, scope, elsewhere, StateConcluded, "other database")}
	for _, r := range runs {
		age(t, ctx, pool, r.ID, 2*time.Minute)
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "root found")
	age(t, ctx, pool, a.ID, 2*time.Minute)
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
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "root found")
	age(t, ctx, pool, a.ID, 2*time.Minute)
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

// CHECK-34: two sidecars running the notifier against one store send once.
func TestConclusionNotifier_TwoSidecarsSendOnce(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)
	st2, err := NewPostgresStore(other, DefaultLimits())
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	key := "test:" + string(NewUUID())
	sinkA, sinkB := &recordingSink{}, &recordingSink{}
	a, scope := followUpCoordinator(t, ctx, st, key, sinkA, NewPGTriggerSource(pool, "orders"))
	b, scope2 := followUpCoordinator(t, ctx, st2, key, sinkB,
		NewPGTriggerSource(other, "orders"))
	if scope != scope2 {
		t.Fatalf("the sidecars bound different scopes: %v / %v", scope, scope2)
	}
	var incidents []string
	for i := 0; i < 4; i++ {
		inc := insertIncident(t, ctx, pool, "orders", "critical")
		incidents = append(incidents, inc)
		age(t, ctx, pool, runFor(t, ctx, st, scope, inc, StateConcluded, "root").ID,
			2*time.Minute)
	}
	var wg sync.WaitGroup
	for round := 0; round < 3; round++ {
		for _, c := range []*Coordinator{a, b} {
			wg.Add(1)
			go func(c *Coordinator) {
				defer wg.Done()
				c.notifyConclusions(ctx, scope)
			}(c)
		}
	}
	wg.Wait()
	sent := map[string]int{}
	for _, s := range append(sinkA.signals(), sinkB.signals()...) {
		sent[s.IncidentID]++
	}
	for _, inc := range incidents {
		if sent[inc] != 1 {
			t.Fatalf("incident %s followed up %d times, want 1 (all: %v)", inc, sent[inc], sent)
		}
	}
	var dupes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT incident_id
		FROM sage.sre_incident_followup GROUP BY 1 HAVING count(*) > 1) d`).
		Scan(&dupes); err != nil || dupes != 0 {
		t.Fatalf("CHECK-34: %d incidents with more than one claim (%v)", dupes, err)
	}
}

// CHECK-36 (signal side): the planted application name and SQL literal
// never reach the signal.
func TestConclusionNotifier_SignalCarriesNoEvidenceOrSQL(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded,
		"pid 7 ran UPDATE accounts SET pin = 'planted-sql-literal' and then idled")
	age(t, ctx, pool, a.ID, 2*time.Minute)
	c.notifyConclusions(ctx, scope)
	got := sink.signals()
	if len(got) != 1 {
		t.Fatalf("%d follow-ups, want 1", len(got))
	}
	text := fmt.Sprintf("%+v", got[0])
	for _, leak := range []string{"planted-app-secret", "billing", "planted-sql-literal"} {
		if strings.Contains(text, leak) {
			t.Fatalf("CHECK-36: the signal carries %q: %s", leak, text)
		}
	}
	if !strings.Contains(got[0].Reason, "[redacted sql_literal]") {
		t.Fatalf("reason = %q, want the literal redacted", got[0].Reason)
	}
}

func TestConclusionNotifier_RunsInTheTriggerTick(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	sink := &recordingSink{}
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), sink,
		NewPGTriggerSource(pool, "orders"))
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "root found")
	age(t, ctx, pool, a.ID, 2*time.Minute)
	last := time.Now()
	c.tick(ctx, &last)
	if got := sink.signals(); len(got) != 1 || got[0].InvestigationID != a.ID {
		t.Fatalf("the trigger tick sent %+v", got)
	}
}

func TestConclusionNotifier_WithoutASinkDoesNothing(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), nil, nil)
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	a := runFor(t, ctx, st, scope, inc, StateConcluded, "root found")
	age(t, ctx, pool, a.ID, 2*time.Minute)
	c.notifyConclusions(ctx, scope)
	if _, ok := claimOf(t, ctx, pool, inc); ok {
		t.Fatal("a coordinator without follow-ups claimed the incident")
	}
}

func TestNewCoordinator_FollowUpsNeedAnIncidentSource(t *testing.T) {
	st, _, _ := liveStore(t, DefaultLimits())
	_, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: newScriptedRunner(),
		Config: DefaultCoordinatorConfig("test:x"), FollowUps: &recordingSink{}})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "incident") {
		t.Fatalf("err = %v, want an invalid request naming the incident source", err)
	}
}

func TestPGTriggerSource_TrackedIncident(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	src := NewPGTriggerSource(pool, "orders")
	inc := insertIncident(t, ctx, pool, "orders", "critical")
	got, ok, err := src.TrackedIncident(ctx, inc)
	if err != nil || !ok || got != (TrackedIncident{Database: "orders", Severity: "critical"}) {
		t.Fatalf("tracked = %+v %v %v", got, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.incidents SET resolved_at = now()
		WHERE id = $1::uuid`, inc); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok, err := src.TrackedIncident(ctx, inc); err != nil || !ok {
		t.Fatalf("a resolved incident is still tracked: %v %v", ok, err)
	}
	other := insertIncident(t, ctx, pool, "payments", "warning")
	for _, id := range []string{other, string(NewUUID()), "inc-not-a-uuid", ""} {
		if got, ok, err := src.TrackedIncident(ctx, id); err != nil || ok {
			t.Fatalf("incident %q: %+v %v %v, want untracked", id, got, ok, err)
		}
	}
	var nilSrc *PGTriggerSource
	if _, _, err := nilSrc.TrackedIncident(ctx, inc); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil source err = %v", err)
	}
}
