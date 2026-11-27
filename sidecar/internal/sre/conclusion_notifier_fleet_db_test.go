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
)

// sre-08: the conclusion notifier across sidecars, its redaction, its place in
// the trigger tick, and the incident source it reads.

// secondSidecarStore is another sidecar's store: its own pool on the same
// database.
func secondSidecarStore(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (
	*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)
	st2, err := NewPostgresStore(other, DefaultLimits())
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	return st2, other
}

// CHECK-34: two sidecars running the notifier against one store send once.
func TestConclusionNotifier_TwoSidecarsSendOnce(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	st2, other := secondSidecarStore(t, ctx, pool)
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
		inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
		incidents = append(incidents, inc)
		run := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root")
		ageConclusion(t, ctx, pool, run.ID, 2*time.Minute)
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
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded,
		"pid 7 ran UPDATE accounts SET pin = 'planted-sql-literal' and then idled")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
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
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root found")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
	last := time.Now()
	c.tick(ctx, &last)
	if got := sink.signals(); len(got) != 1 || got[0].InvestigationID != a.ID {
		t.Fatalf("the trigger tick sent %+v", got)
	}
}

func TestConclusionNotifier_WithoutASinkDoesNothing(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	c, scope := followUpCoordinator(t, ctx, st, "test:"+string(NewUUID()), nil, nil)
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
	a := runOfIncident(t, ctx, st, scope, inc, StateConcluded, "root found")
	ageConclusion(t, ctx, pool, a.ID, 2*time.Minute)
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
	inc := insertFollowUpIncident(t, ctx, pool, "orders", "critical")
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
	other := insertFollowUpIncident(t, ctx, pool, "payments", "warning")
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
