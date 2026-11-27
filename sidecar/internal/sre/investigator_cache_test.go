package sre

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// sre-11: graph_state answers from the session's evidence cache (seeded
// once from the store, then appended with each committed read), which
// must diagnose exactly as a full re-read would. The cache never feeds
// the conclusion: finish re-reads and re-verifies every cited hash.
//
// No concurrency test here: the cache is owned by one session goroutine
// (the agent loop runs tools in order); two workers on one investigation
// are fenced by the lease, which the store's fence tests cover.

// cacheStore wraps a store, failing StepEvidence or Evidence on demand.
type cacheStore struct {
	CoordinatorStore
	stepErr     error
	stepEmpty   bool
	evidenceErr error
}

func (s *cacheStore) StepEvidence(ctx context.Context, scope Scope, id UUID,
	key string) ([]Evidence, error) {
	if s.stepErr != nil || s.stepEmpty {
		return nil, s.stepErr
	}
	return s.CoordinatorStore.StepEvidence(ctx, scope, id, key)
}

func (s *cacheStore) Evidence(ctx context.Context, scope Scope, id UUID) ([]Evidence, error) {
	if s.evidenceErr != nil {
		return nil, s.evidenceErr
	}
	return s.CoordinatorStore.Evidence(ctx, scope, id)
}

// cacheSession is an investigator session on a claimed investigation in
// evaluating state, as collect hands it to the investigator.
func cacheSession(t *testing.T, ctx context.Context, st *PostgresStore, store CoordinatorStore,
	req StartRequest) *investigatorSession {
	t.Helper()
	inv, lease := claimedEvaluating(t, ctx, st, req)
	return &investigatorSession{c: &Coordinator{store: store}, lease: lease, inv: inv}
}

// assertCacheMatchesStore checks graph_state's diagnosis and text on the
// cache equal the diagnosis of a full re-read.
func assertCacheMatchesStore(t *testing.T, ctx context.Context, s *investigatorSession,
	label string) {
	t.Helper()
	cached, err := s.graphDiagnosis(ctx)
	if err != nil {
		t.Fatalf("%s: cached diagnosis: %v", label, err)
	}
	full, stored, err := s.diagnosis(ctx)
	if err != nil {
		t.Fatalf("%s: full diagnosis: %v", label, err)
	}
	if !reflect.DeepEqual(cached, full) {
		t.Fatalf("%s: cached diagnosis %+v differs from the full read's %+v", label,
			cached, full)
	}
	if ids, want := evidenceIDs(s.observed), evidenceIDs(stored); !reflect.DeepEqual(ids,
		want) {
		t.Fatalf("%s: cache holds %v, the store %v", label, ids, want)
	}
	out, err := s.graphTool().Run(ctx, nil)
	if err != nil || out.Text != graphText(full) {
		t.Fatalf("%s: graph_state = %q (%v), want %q", label, out.Text, err, graphText(full))
	}
}

func evidenceIDs(ev []Evidence) []UUID {
	out := make([]UUID, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.ID)
	}
	return out
}

var replayTriggers = map[string]TriggerKind{"lock_blocking": TriggerLock,
	"connection_pressure": TriggerConnections, "plan_regression": TriggerPlan,
	"wal_retention": TriggerWAL}

// replayResult is one recorded observation as a probe returns it. A
// source outside the catalog (the pooler's pools) keeps version v1.
func replayResult(c replay.Case, o replay.Observation) probes.Result {
	version := "v1"
	if spec, ok := probes.Catalog().Spec(o.Probe); ok {
		version = spec.Version
	}
	return probes.Result{ProbeID: o.Probe, Version: version, Status: o.Status,
		Reason: o.Reason, Error: o.Error, ObservedAt: c.At(o), Truncated: o.Truncated,
		Rows: o.Rows}
}

func TestInvestigatorCache_MatchesTheFullReadOnTheReplayCorpus(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	cases, err := replay.Corpus()
	if err != nil || len(cases) == 0 {
		t.Fatalf("corpus: %d cases (%v)", len(cases), err)
	}
	for _, c := range cases {
		kind, ok := replayTriggers[c.Family]
		if !ok {
			t.Fatalf("case %s: family %q has no trigger kind", c.ID, c.Family)
		}
		s := cacheSession(t, ctx, st, st, StartRequest{Scope: testScope(t, ctx, st),
			CaseID: "replay:" + c.ID, TriggerKind: kind, Subject: c.Subject})
		assertCacheMatchesStore(t, ctx, s, c.ID+" before any read")
		for i, o := range c.Observations {
			if _, err := s.commit(ctx, replayResult(c, o)); err != nil {
				t.Fatalf("case %s read %d: commit: %v", c.ID, i+1, err)
			}
			assertCacheMatchesStore(t, ctx, s, c.ID+" after read "+itoa(int64(i+1)))
		}
	}
}

// A read committed before graph_state is first asked for is in the seed
// and is not appended again.
func TestInvestigatorCache_ReadsBeforeTheSeedAreHeldOnce(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	s := cacheSession(t, ctx, st, st, lockStart(testScope(t, ctx, st), "pid 1110"))
	for _, id := range []probes.ID{probes.LockGraph, probes.LongTransactions} {
		if _, err := s.commit(ctx, probeResult(id)); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}
	assertCacheMatchesStore(t, ctx, s, "seeded after 2 reads")
	if _, err := s.commit(ctx, probeResult(probes.PreparedXacts)); err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertCacheMatchesStore(t, ctx, s, "after a third read")
	if len(s.observed) != 3 {
		t.Fatalf("cache holds %d rows, want 3", len(s.observed))
	}
}

func TestInvestigatorCache_RetriedCommitIsCachedOnce(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	s := cacheSession(t, ctx, st, st, lockStart(testScope(t, ctx, st), "pid 1111"))
	assertCacheMatchesStore(t, ctx, s, "seeded empty")
	first, err := s.commit(ctx, probeResult(probes.LongTransactions, probes.Row{"pid": int64(7)}))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	s.commits = 0 // the retry reuses the idempotency key of the first commit
	again, err := s.commit(ctx, probeResult(probes.LongTransactions,
		probes.Row{"pid": int64(8)}))
	if err != nil {
		t.Fatalf("retried commit: %v", err)
	}
	if again.ID != first.ID || !reflect.DeepEqual(again.SHA256, first.SHA256) {
		t.Fatalf("retried commit returned %s, want the original row %s", again.ID, first.ID)
	}
	if len(s.observed) != 1 {
		t.Fatalf("cache holds %d rows after a retried commit, want 1", len(s.observed))
	}
	assertCacheMatchesStore(t, ctx, s, "after the retry")
}

func TestInvestigatorCache_ResumedRunSeedsFromTheStore(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	s := cacheSession(t, ctx, st, st, lockStart(testScope(t, ctx, st), "pid 1112"))
	assertCacheMatchesStore(t, ctx, s, "first segment seeded")
	for _, id := range []probes.ID{probes.LockGraph, probes.LongTransactions} {
		if _, err := s.commit(ctx, probeResult(id)); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}
	earlier := evidenceIDs(s.observed)
	if _, err := st.Release(ctx, s.lease, StateNeedsEvidence); err != nil {
		t.Fatalf("release: %v", err)
	}
	lease, err := st.Claim(ctx, s.inv.Scope, s.inv.ID, NewUUID())
	if err != nil || lease.Fence <= s.lease.Fence {
		t.Fatalf("re-claim = fence %d (%v), want above %d", lease.Fence, err, s.lease.Fence)
	}
	if _, err := st.CommitStep(ctx, lease, step("evaluate-f2", StateEvaluating)); err != nil {
		t.Fatalf("move to evaluating: %v", err)
	}
	resumed := &investigatorSession{c: s.c, lease: lease, inv: s.inv}
	assertCacheMatchesStore(t, ctx, resumed, "resumed seed")
	if got := evidenceIDs(resumed.observed); !reflect.DeepEqual(got, earlier) {
		t.Fatalf("resumed cache = %v, want the earlier segment's %v", got, earlier)
	}
	ev, err := resumed.commit(ctx, probeResult(probes.PreparedXacts))
	if err != nil || ev.StepKey != "inv-read-f"+itoa(lease.Fence)+"-1" {
		t.Fatalf("resumed commit = %+v (%v), want a read keyed by the new fence", ev, err)
	}
	assertCacheMatchesStore(t, ctx, resumed, "after a resumed read")
	if len(resumed.observed) != 3 {
		t.Fatalf("resumed cache holds %d rows, want 3", len(resumed.observed))
	}
}

func TestInvestigatorCache_CommitReportsAReadThatIsNotStored(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	boom := errors.New("store unavailable")
	for name, wrap := range map[string]*cacheStore{
		"not found": {stepErr: ErrNotFound}, "no rows": {stepEmpty: true}} {
		wrap.CoordinatorStore = st
		s := cacheSession(t, ctx, st, wrap, lockStart(testScope(t, ctx, st), "pid "+name))
		_, err := s.commit(ctx, probeResult(probes.LockGraph))
		if !errors.Is(err, ErrNotFound) ||
			!strings.Contains(err.Error(), "the committed read inv-read-f") ||
			!strings.Contains(err.Error(), "is not stored") {
			t.Errorf("%s: commit err = %v, want the read reported as not stored", name, err)
		}
	}
	wrap := &cacheStore{CoordinatorStore: st, stepErr: boom}
	s := cacheSession(t, ctx, st, wrap, lockStart(testScope(t, ctx, st), "pid 1113"))
	if _, err := s.commit(ctx, probeResult(probes.LockGraph)); !errors.Is(err, boom) ||
		errors.Is(err, ErrNotFound) {
		t.Errorf("store failure: commit err = %v, want the store's error", err)
	}
}

func TestInvestigatorCache_SeedFailureAbortsGraphState(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	boom := errors.New("store unavailable")
	wrap := &cacheStore{CoordinatorStore: st, evidenceErr: boom}
	s := cacheSession(t, ctx, st, wrap, lockStart(testScope(t, ctx, st), "pid 1114"))
	_, err := s.graphTool().Run(ctx, nil)
	var abort interface{ Unwrap() error }
	if !errors.Is(err, boom) || !errors.As(err, &abort) {
		t.Fatalf("graph_state err = %v, want the store failure as an abort", err)
	}
	wrap.evidenceErr = nil // a later call seeds once the store answers
	assertCacheMatchesStore(t, ctx, s, "after the store recovered")
}

// tamperLatest rewrites the newest stored row of a probe behind the
// append-only trigger, as a corrupted or edited table would.
func tamperLatest(t *testing.T, pool *pgxpool.Pool, id probes.ID) UUID {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Errorf("tamper: begin: %v", err)
		return ""
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var got string
	err = tx.QueryRow(ctx, `SELECT id::text FROM sage.sre_evidence WHERE probe_id = $1
		ORDER BY collected_at DESC, id LIMIT 1`, string(id)).Scan(&got)
	if err == nil {
		_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE sage.sre_evidence
			SET payload = jsonb_set(payload, '{reason}', '"tampered"') WHERE id = $1::uuid`, got)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		t.Errorf("tamper %s: %v", id, err)
	}
	return UUID(got)
}

func TestInvestigatorCache_TamperedPayloadIsDroppedAtFinish(t *testing.T) {
	st, pool, ctx := liveStore(t, budgetLimits())
	var tampered UUID
	final := submit(func(body string) invFinal {
		return invFinal{Outcome: "agree", Root: "idle_in_tx_holder", Claims: []invClaim{
			idleClaim(aliasOf(t, body, probes.LockGraph, "ok")),
			{Text: "pid 4242 has kept its transaction open for 95 s.",
				EvidenceIDs: []string{lastAliasOf(t, body, probes.LongTransactions, "ok")}}}}
	})
	m := newFakeModel(t, call(ToolRunProbe, probeArgs(probes.LongTransactions)),
		call(ToolGraphState, `{}`),
		func(w http.ResponseWriter, body string) {
			tampered = tamperLatest(t, pool, probes.LongTransactions)
			final(w, body)
		})
	c, _ := investigatorCoordinator(t, ctx, st, longTxRunner(), m.client(), invOptions{})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-tamper"))
	reads := stepsOf(transcriptOf(t, inv), ToolRunProbe)
	if tampered == "" || len(reads) != 1 || reads[0].EvidenceID != tampered {
		t.Fatalf("tampered %s, reads %+v: want the run_probe read tampered", tampered, reads)
	}
	if ev := evidenceByID(t, st, inv)[tampered]; ev.VerifyHash() {
		t.Fatalf("tampered row %s still verifies", tampered)
	}
	n := inv.Summary.Narrative
	if n == nil || len(n.Claims) != 1 {
		t.Fatalf("narrative = %+v, want only the untampered claim", n)
	}
	for _, id := range n.Claims[0].EvidenceIDs {
		if id == tampered {
			t.Fatalf("kept claim %q cites the tampered row %s", n.Claims[0].Text, id)
		}
	}
}
