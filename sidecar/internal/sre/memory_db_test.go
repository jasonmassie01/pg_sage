package sre

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Incident memory against real PostgreSQL. The leakage guard: an
// investigation never retrieves itself, anything created after its own
// start, anything concluded after its start, an outcome recorded after
// its start, or an earlier investigation of the same case or incident (a
// replayed incident must not see its own answer). Retrieval stays in one
// database and one family.

func similarTo(t *testing.T, st *PostgresStore, inv Investigation) []SimilarIncident {
	t.Helper()
	got, err := st.Get(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	hs, err := st.Hypotheses(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("hypotheses: %v", err)
	}
	latest, _ := latestRevision(hs)
	out, err := st.SimilarInvestigations(t.Context(), inv.Scope, SimilarQuery{Target: got,
		Family: got.Summary.Family, Features: recordFeatures(latest, got.Summary), Limit: 10})
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	return out
}

func ids(items []SimilarIncident) map[UUID]bool {
	out := map[UUID]bool{}
	for _, s := range items {
		out[s.InvestigationID] = true
	}
	return out
}

func TestSimilar_NeverRetrievesItselfOrTheFuture(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	a := startAndRun(t, ctx, c, lockTrigger("mem-a"))
	b := startAndRun(t, ctx, c, lockTrigger("mem-b"))
	target := startAndRun(t, ctx, c, lockTrigger("mem-t"))
	later := startAndRun(t, ctx, c, lockTrigger("mem-c"))
	got := ids(similarTo(t, st, target))
	if !got[a.ID] || !got[b.ID] || got[target.ID] || got[later.ID] || len(got) != 2 {
		t.Fatalf("similar to the target = %v; want a and b only (never itself or later)",
			got)
	}
	if first := similarTo(t, st, a); len(first) != 0 {
		t.Fatalf("the first investigation retrieved %+v; everything else came later", first)
	}
	for _, s := range similarTo(t, st, target) {
		if s.Label != SimilarLabel || s.Root != "idle_in_tx_holder" || s.Score != 1 ||
			s.State != StateConcluded || s.Outcome != nil || s.ConcludedAt.IsZero() {
			t.Fatalf("similar incident = %+v, want a labeled, unverified idle-holder match", s)
		}
	}
}

func TestSimilar_ExcludesWorkConcludedAfterTheStart(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	early, _, err := c.Start(ctx, lockTrigger("mem-early"))
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := c.Start(ctx, lockTrigger("mem-target"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []UUID{early.ID, target.ID} {
		if err := c.Investigate(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.Get(ctx, target.Scope, target.ID)
	if s := similarTo(t, st, got); len(s) != 0 {
		t.Fatalf("retrieved %+v, which concluded after the target started", s)
	}
}

// The same guard when the past incident is only described by its summary
// (missing evidence, no supported hypothesis): its summary was written
// after the target started, so it must not be read.
func TestSimilar_ExcludesSummaryOnlyWorkConcludedAfterTheStart(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	blind := newScriptedRunner().script(probes.LockGraph, lockGraphTimeout())
	c, _ := testCoordinator(t, ctx, st, blind, nil)
	done := startAndRun(t, ctx, c, lockTrigger("mem-blind-past"))
	early, _, _ := c.Start(ctx, lockTrigger("mem-blind-early"))
	target, _, _ := c.Start(ctx, lockTrigger("mem-blind-target"))
	for _, id := range []UUID{early.ID, target.ID} {
		if err := c.Investigate(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.Get(ctx, target.Scope, target.ID)
	ids := ids(similarTo(t, st, got))
	if ids[early.ID] || !ids[done.ID] {
		t.Fatalf("similar = %v; want the earlier blind investigation %s and not %s, "+
			"which concluded after the target started", ids, done.ID, early.ID)
	}
}

func TestSimilar_OutcomeRecordedAfterTheStartIsHidden(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	past := startAndRun(t, ctx, c, lockTrigger("mem-past"))
	if _, err := st.RecordOutcome(ctx, past.Scope, past.ID, OutcomeRequest{
		Verdict: "confirmed", Actor: "user:4"}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	target := startAndRun(t, ctx, c, lockTrigger("mem-now"))
	if _, err := st.RecordOutcome(ctx, past.Scope, past.ID, OutcomeRequest{
		Verdict: "refuted", ActualNode: "ddl_lock_queue", Actor: "user:4"}); err != nil {
		t.Fatalf("record later outcome: %v", err)
	}
	got := similarTo(t, st, target)
	if len(got) != 1 || got[0].Outcome == nil || got[0].Outcome.Verdict != OutcomeConfirmed ||
		got[0].Outcome.Actor != "user:4" {
		t.Fatalf("similar = %+v; want the outcome known at the target's start (confirmed)",
			got)
	}
	newest := startAndRun(t, ctx, c, lockTrigger("mem-next"))
	for _, s := range similarTo(t, st, newest) {
		if s.InvestigationID == past.ID && (s.Outcome == nil ||
			s.Outcome.Verdict != OutcomeRefuted || s.Outcome.ActualNode != "ddl_lock_queue") {
			t.Fatalf("a later investigation sees %+v, want the latest (refuted) outcome", s)
		}
	}
}

func TestSimilar_ExcludesTheSameCaseOrIncident(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	original := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:lock:42",
		IncidentID: "42", Kind: TriggerLock, Subject: "incident 42",
		IdempotencyKey: "incident:42"})
	otherIncident := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:lock:43",
		IncidentID: "43", Kind: TriggerLock, Subject: "incident 43"})
	replay := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:lock:42",
		IncidentID: "42", Kind: TriggerLock, Subject: "replay of incident 42"})
	got := ids(similarTo(t, st, replay))
	if got[original.ID] || !got[otherIncident.ID] {
		t.Fatalf("replay retrieved %v; it must not see the original of its own incident",
			got)
	}
	caseReplay := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:lock:42",
		Kind: TriggerLock, Subject: "case replay without an incident id"})
	if got := ids(similarTo(t, st, caseReplay)); got[original.ID] || got[replay.ID] {
		t.Fatalf("case replay retrieved %v; the same case must be excluded", got)
	}
	sameIncident := startAndRun(t, ctx, c, Trigger{CaseID: "operator:replay-42",
		IncidentID: "42", Kind: TriggerLock, Subject: "operator replay 42"})
	if got := ids(similarTo(t, st, sameIncident)); got[original.ID] || got[replay.ID] {
		t.Fatalf("operator replay retrieved %v; same incident id must be excluded", got)
	}
}

func TestSimilar_StaysInOneDatabaseAndFamily(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	other, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	startAndRun(t, ctx, other, lockTrigger("mem-elsewhere"))
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	wal := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:wal:1", IncidentID: "w1",
		Kind: TriggerWAL, Subject: "incident w1"})
	target := startAndRun(t, ctx, c, lockTrigger("mem-here"))
	if got := similarTo(t, st, target); len(got) != 0 {
		t.Fatalf("retrieved %+v from another database or family (wal %s)", got, wal.ID)
	}
}

func TestOutcome_RecordAndValidate(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	done := startAndRun(t, ctx, c, lockTrigger("out-done"))
	o, err := st.RecordOutcome(ctx, done.Scope, done.ID, OutcomeRequest{
		Verdict: "refuted", ActualNode: "prepared_xact_holder", Actor: "user:5"})
	if err != nil || o.Verdict != OutcomeRefuted || o.ActualNode != "prepared_xact_holder" ||
		o.Actor != "user:5" || o.RecordedAt.IsZero() {
		t.Fatalf("outcome = %+v, %v", o, err)
	}
	queued, _, _ := c.Start(ctx, lockTrigger("out-queued"))
	cases := map[string]struct {
		id  UUID
		req OutcomeRequest
		err error
	}{
		"live investigation": {queued.ID, OutcomeRequest{Verdict: "confirmed",
			Actor: "u"}, ErrInvalidRequest},
		"unknown verdict": {done.ID, OutcomeRequest{Verdict: "maybe", Actor: "u"},
			ErrInvalidRequest},
		"unknown node": {done.ID, OutcomeRequest{Verdict: "refuted", ActualNode: "elves",
			Actor: "u"}, ErrInvalidRequest},
		"no actor": {done.ID, OutcomeRequest{Verdict: "confirmed"}, ErrInvalidRequest},
		"not found": {NewUUID(), OutcomeRequest{Verdict: "confirmed", Actor: "u"},
			ErrNotFound},
		"malformed id": {"x", OutcomeRequest{Verdict: "confirmed", Actor: "u"},
			ErrInvalidRequest},
		"confirmed unknown node": {done.ID, OutcomeRequest{Verdict: "confirmed",
			ActualNode: "x", Actor: "u"}, ErrInvalidRequest},
	}
	for name, tc := range cases {
		if _, err := st.RecordOutcome(ctx, done.Scope, tc.id, tc.req); !errors.Is(err, tc.err) {
			t.Errorf("%s: RecordOutcome = %v, want %v", name, err, tc.err)
		}
	}
	inconclusive := startAndRun(t, ctx, c, Trigger{CaseID: "incident:db:wal:9",
		Kind: TriggerWAL, Subject: "incident w9"})
	if _, err := st.RecordOutcome(ctx, done.Scope, inconclusive.ID, OutcomeRequest{
		Verdict: "confirmed", Actor: "u"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("confirming an inconclusive investigation without a node = %v", err)
	}
	other := testScope(t, ctx, st)
	if _, err := st.RecordOutcome(ctx, other, done.ID, OutcomeRequest{Verdict: "confirmed",
		Actor: "u"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("outcome from another database = %v, want ErrNotFound", err)
	}
}

func TestModelTurn_OffersSimilarIncidentsAsFencedContext(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)), toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	first := startAndRun(t, ctx, c, lockTrigger("mem-model-1"))
	if strings.Contains(promptOf(t, m.body(t, 0)), "past_incidents") {
		t.Fatal("the first investigation was offered past incidents")
	}
	if first.Summary.Memory != nil {
		t.Fatalf("first summary memory = %+v, want none", first.Summary.Memory)
	}
	second := startAndRun(t, ctx, c, lockTrigger("mem-model-2"))
	prompt := promptOf(t, m.body(t, 1))
	start := strings.Index(prompt, `<data label="past_incidents">`)
	if start < 0 || !strings.Contains(prompt[start:], "P1:") ||
		!strings.Contains(prompt[start:], "idle_in_tx_holder") {
		t.Fatalf("second prompt lacks the fenced past incident:\n%s", prompt)
	}
	mem := second.Summary.Memory
	if mem == nil || mem.Label != MemoryLabel || len(mem.InvestigationIDs) != 1 ||
		mem.InvestigationIDs[0] != first.ID {
		t.Fatalf("summary memory = %+v, want the first investigation", mem)
	}
	if second.Summary.Narrative == nil {
		t.Fatal("the model's narrative was lost when memory was offered")
	}
}

// promptOf is the user message of a chat completions request body.
func promptOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "user" {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

// failingMemory is a store whose memory lookup fails.
type failingMemory struct{ *PostgresStore }

func (failingMemory) SimilarInvestigations(context.Context, Scope,
	SimilarQuery) ([]SimilarIncident, error) {
	return nil, ErrMetadataUnavailable
}

func TestModelTurn_MemoryFailureKeepsTheModelTurn(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	plain, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	startAndRun(t, ctx, plain, lockTrigger("mem-fail-past"))
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	logs := &logLines{}
	cfg := plain.cfg
	c, err := NewCoordinator(CoordinatorDeps{Store: failingMemory{st}, Runner: idleChainRunner(),
		Config: cfg, Model: m.client(), Notices: &OnceLog{}, LogFn: logs.logFn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv := startAndRun(t, ctx, c, lockTrigger("mem-fail-now"))
	if inv.State != StateConcluded || inv.Summary.Narrative == nil ||
		inv.Summary.Memory != nil {
		t.Fatalf("investigation %s narrative %v memory %+v; a memory failure must only "+
			"drop the memory", inv.State, inv.Summary.Narrative, inv.Summary.Memory)
	}
	if logs.count("similar") == 0 {
		t.Fatal("the failed memory lookup was not logged")
	}
	if strings.Contains(promptOf(t, m.body(t, 0)), "past_incidents") {
		t.Fatal("past incidents were offered although the lookup failed")
	}
}
