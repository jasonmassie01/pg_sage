package sre

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Roadmap 2.4 in a real investigation: the model ranks ddl_lock_queue
// above the graph's conclusive root idle_in_tx_holder. Without earned
// root authority the graph's root stands and the contest is stored as
// advisory; with it, the model's root is adopted. The authority is asked
// only on a disagreement, once, for the investigation's family; its
// failure never fails the investigation.

type fakeAuthority struct {
	mu      sync.Mutex
	grant   RootGrant
	err     error
	asked   []string
	blockOn chan struct{}
}

func (f *fakeAuthority) ModelRootAuthority(ctx context.Context, family string) (RootGrant,
	error) {
	f.mu.Lock()
	f.asked = append(f.asked, family)
	f.mu.Unlock()
	if f.blockOn != nil {
		select {
		case <-f.blockOn:
		case <-ctx.Done():
			return RootGrant{}, ctx.Err()
		}
	}
	return f.grant, f.err
}

func (f *fakeAuthority) families() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// authorityCoordinator is modelCoordinator with a root authority.
func authorityCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	model *llm.Client, auth RootAuthority) (*Coordinator, *logLines) {
	t.Helper()
	logs := &logLines{}
	cfg := DefaultCoordinatorConfig("test:" + string(NewUUID()))
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: idleChainRunner(),
		Config: cfg, Model: model, Notices: &OnceLog{}, LogFn: logs.logFn,
		RootAuthority: auth})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c, logs
}

// disagreeingModel ranks ddl_lock_queue above the graph's root and cites
// the lock graph.
func disagreeingModel(t *testing.T) *fakeModel {
	return newFakeModel(t, toolReply(func(body string) string {
		return wireReview{Ranking: []string{"ddl_lock_queue", "idle_in_tx_holder"},
			Claims: []wireClaim{idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}.json()
	}))
}

func statusesOf(t *testing.T, st *PostgresStore, inv Investigation) map[string]HypothesisStatus {
	t.Helper()
	hs, err := st.Hypotheses(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("hypotheses: %v", err)
	}
	out := map[string]HypothesisStatus{}
	for _, h := range hs {
		out[h.Node] = h.Status
	}
	return out
}

func TestRootAuthority_NoneKeepsTheGraphRootAndRecordsAnAdvisoryContest(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	c, _ := authorityCoordinator(t, ctx, st, disagreeingModel(t).client(), nil)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-none"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	mc := inv.Summary.ModelContest
	if mc == nil || mc.Label != ModelContestLabel || mc.GraphRoot != "idle_in_tx_holder" ||
		mc.ModelRoot != "ddl_lock_queue" || mc.Authority != ContestAdvisory ||
		!strings.Contains(mc.Reason, "no root authority") {
		t.Fatalf("contest = %+v", mc)
	}
	got := payloads(t, st, inv, EventModelDisagreed)
	if len(got) != 1 || got[0]["authority"] != ContestAdvisory ||
		got[0]["graph_root"] != "idle_in_tx_holder" || got[0]["model_root"] != "ddl_lock_queue" {
		t.Fatalf("model_disagreed = %v", got)
	}
}

func TestRootAuthority_DeniedKeepsTheGraphRootWithTheReason(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Reason: "lower bound 0.79 below 0.80"}}
	c, _ := authorityCoordinator(t, ctx, st, disagreeingModel(t).client(), auth)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-denied"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	mc := inv.Summary.ModelContest
	if mc == nil || mc.Authority != ContestAdvisory || !strings.Contains(mc.Reason, "0.79") {
		t.Fatalf("contest = %+v", mc)
	}
	if fams := auth.families(); len(fams) != 1 || fams[0] != "lock_blocking" {
		t.Fatalf("authority asked for %v, want [lock_blocking] once", fams)
	}
}

func TestRootAuthority_GrantedAdoptsTheModelRoot(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true,
		Reason: "override precision 34/36, lower bound 0.82 >= 0.80 (held-out, live)"}}
	c, _ := authorityCoordinator(t, ctx, st, disagreeingModel(t).client(), auth)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-granted"))
	if inv.State != StateConcluded || inv.Summary.Root != "ddl_lock_queue" {
		t.Fatalf("investigation = %s root %q, want the adopted ddl_lock_queue", inv.State,
			inv.Summary.Root)
	}
	statuses := statusesOf(t, st, inv)
	if statuses["ddl_lock_queue"] != HypothesisRoot ||
		statuses["idle_in_tx_holder"] != HypothesisContributing {
		t.Fatalf("statuses = %v", statuses)
	}
	mc := inv.Summary.ModelContest
	if mc == nil || mc.Authority != ContestAdopted || mc.GraphRoot != "idle_in_tx_holder" ||
		!strings.Contains(mc.Reason, "lower bound") {
		t.Fatalf("contest = %+v", mc)
	}
	if inv.Summary.ModelRanking == nil || inv.Summary.ModelRanking.Nodes[0] != "ddl_lock_queue" ||
		inv.Summary.Narrative == nil {
		t.Fatalf("the adopted review must be stored: %+v", inv.Summary)
	}
	dis := payloads(t, st, inv, EventModelDisagreed)
	if len(dis) != 1 || dis[0]["authority"] != ContestAdopted {
		t.Fatalf("model_disagreed = %v", dis)
	}
	if len(payloads(t, st, inv, EventModelReviewed)) != 1 {
		t.Fatal("an adopted review must also be recorded as reviewed")
	}
}

func TestRootAuthority_ErrorIsAdvisoryAndLogged(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{err: errors.New("ledger unavailable: connection refused")}
	c, logs := authorityCoordinator(t, ctx, st, disagreeingModel(t).client(), auth)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-error"))
	assertIdleRoot(t, st, inv)
	mc := inv.Summary.ModelContest
	if mc == nil || mc.Authority != ContestAdvisory ||
		!strings.Contains(mc.Reason, "unavailable") {
		t.Fatalf("contest = %+v", mc)
	}
	if logs.count("root authority") == 0 {
		t.Fatal("the authority failure must be logged")
	}
	if strings.Contains(mc.Reason, "connection refused") {
		t.Fatalf("the stored reason must not carry the raw error: %q", mc.Reason)
	}
}

func TestRootAuthority_NotAskedWhenTheModelAgrees(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "x"}}
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := authorityCoordinator(t, ctx, st, m.client(), auth)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-agree"))
	assertIdleRoot(t, st, inv)
	if inv.Summary.ModelContest != nil || len(auth.families()) != 0 {
		t.Fatalf("contest %+v, asked %v", inv.Summary.ModelContest, auth.families())
	}
}

func TestRootAuthority_SlowAuthorityIsBoundedByTheInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "x"},
		blockOn: make(chan struct{})}
	c, _ := authorityCoordinator(t, ctx, st, disagreeingModel(t).client(), auth)
	start := time.Now()
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-slow"))
	if time.Since(start) > rootAuthorityTimeout+20*time.Second {
		t.Fatalf("a blocked authority held the investigation for %s", time.Since(start))
	}
	assertIdleRoot(t, st, inv)
	if mc := inv.Summary.ModelContest; mc == nil || mc.Authority != ContestAdvisory {
		t.Fatalf("contest = %+v", mc)
	}
}
