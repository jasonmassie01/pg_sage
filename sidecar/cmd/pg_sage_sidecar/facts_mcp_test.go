package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/notify"
)

// facts-09: every proposal path (MCP propose_fact, mark_object, the
// worker, the first look) sends exactly one approval_needed fact card,
// through the one notifier the runtime registers on its database pool.

type cardRules struct{ expiryRules }

func (*cardRules) MatchingRules(_ context.Context, event string) ([]notify.Rule, error) {
	if event != "approval_needed" {
		return nil, nil
	}
	return []notify.Rule{{ID: 1, ChannelID: 2, Event: event, MinSeverity: "warning",
		Enabled: true}}, nil
}

type cardSender struct {
	mu   sync.Mutex
	sent []notify.Event
}

func (s *cardSender) Type() string { return "email" }
func (s *cardSender) Send(_ context.Context, _ notify.Channel, evt notify.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, evt)
	return nil
}

// cards returns the fact ids of the fact cards sent so far.
func (s *cardSender) cards() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for _, evt := range s.sent {
		if ref, ok := notify.FactCardOf(evt); ok && evt.Type == "approval_needed" {
			ids = append(ids, ref.FactID)
		}
	}
	return ids
}

type cardRig struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	rt     *databaseRuntime
	sender *cardSender
	mcp    factsMCPBackend
}

// newCardRig is a runtime over a fresh database with a dispatcher whose
// approval_needed rule delivers to a recording channel.
func newCardRig(t *testing.T, rules notify.RuleStore) *cardRig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool := clusterPool(t, ctx, "factcards")
	sender := &cardSender{}
	d := notify.NewDispatcherWithStore(rules, func(string, string, ...any) {})
	d.RegisterSender(sender)
	rt := &databaseRuntime{cfg: config.DefaultConfig(), dispatcher: d,
		spec: databaseRuntimeSpec{Name: "orders", Pool: pool}}
	rt.ctx, rt.cancel = context.WithCancel(ctx)
	t.Cleanup(rt.cancel)
	rt.facts = newFactStore(pool)
	rt.registerFactCards()
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Config: config.DatabaseConfig{Name: "orders"}, Status: &fleet.InstanceStatus{}})
	return &cardRig{ctx: ctx, pool: pool, rt: rt, sender: sender,
		mcp: factsMCPBackend{manager: mgr}}
}

func (r *cardRig) proposeOverMCP(t *testing.T, subject string) facts.Fact {
	t.Helper()
	out, err := r.mcp.ProposeFact(r.ctx, mcp.FactRequest{Database: "orders",
		Type: "test_fixture", SubjectKind: "schema", Subject: subject,
		Evidence: "copies left by CI"}, "agent:claude-code")
	if err != nil {
		t.Fatalf("MCP propose_fact %s: %v", subject, err)
	}
	return out.(map[string]any)["fact"].(facts.Fact)
}

func TestFactCardMCPProposeSendsOneCard(t *testing.T) {
	r := newCardRig(t, &cardRules{})
	f := r.proposeOverMCP(t, "test_mcp_*")
	if got := r.sender.cards(); len(got) != 1 || got[0] != f.ID {
		t.Fatalf("MCP proposal sent cards %v, want [%d]", got, f.ID)
	}
	evt := r.sender.sent[0]
	if evt.Severity != "warning" || evt.Data["database"] != "orders" ||
		!strings.Contains(evt.Body, "agent:claude-code") {
		t.Fatalf("card = %+v", evt)
	}
	again := r.proposeOverMCP(t, "test_mcp_*")
	if again.ID != f.ID || again.Proposals != 2 {
		t.Fatalf("re-proposal: %+v", again)
	}
	if got := r.sender.cards(); len(got) != 1 {
		t.Fatalf("re-proposing the same fact sent another card: %v", got)
	}
}

func TestFactCardMarkObjectSendsOneCard(t *testing.T) {
	r := newCardRig(t, &cardRules{})
	tools := agenttools.New(r.pool, agenttools.Options{})
	req := agenttools.MarkRequest{Kind: "schema", Subject: "app_mark_*", Mark: "owned",
		Repo: "github.com/acme/app", Evidence: "every app schema comes from migrations"}
	res, err := tools.MarkObject(r.ctx, req, "agent:claude-code")
	if err != nil || !res.Created {
		t.Fatalf("mark_object: %+v %v", res, err)
	}
	if got := r.sender.cards(); len(got) != 1 || got[0] != res.FactID {
		t.Fatalf("mark_object sent cards %v, want [%d]", got, res.FactID)
	}
	if _, err := tools.MarkObject(r.ctx, req, "agent:claude-code"); err != nil {
		t.Fatal(err)
	}
	if got := r.sender.cards(); len(got) != 1 {
		t.Fatalf("a repeated mark sent another card: %v", got)
	}
}

type oneFixture struct{ subject string }

func (d oneFixture) Name() string { return "fixture" }
func (d oneFixture) Detect(context.Context) ([]facts.Proposal, error) {
	return []facts.Proposal{{Type: facts.TypeTestFixture, Kind: facts.KindSchema,
		Subject: d.subject, Source: facts.SourceDetector,
		Evidence: []facts.Citation{{Kind: "catalog", Ref: "schemas:" + d.subject}}}}, nil
}

// Regression: the worker's fact sends one card, not two (the worker no
// longer notifies on its own), and so does the first look's.
func TestFactCardWorkerAndFirstLookSendOneCardEach(t *testing.T) {
	r := newCardRig(t, &cardRules{})
	opts := r.rt.factWorkerOptions()
	opts.Detectors = append(opts.Detectors, oneFixture{subject: "test_worker_*"})
	w := facts.NewWorker(r.rt.facts, opts)
	if stats := w.RunOnce(r.ctx); stats.Proposed != 1 {
		t.Fatalf("worker pass: %+v", stats)
	}
	if got := r.sender.cards(); len(got) != 1 {
		t.Fatalf("one worker fact sent %d cards (%v), want 1", len(got), got)
	}
	w.RunOnce(r.ctx)
	if got := r.sender.cards(); len(got) != 1 {
		t.Fatalf("the second pass sent another card: %v", got)
	}
	run := &firstLookRun{name: "orders", facts: r.rt.facts, logf: logStructuredWrapper}
	props, _ := oneFixture{subject: "test_first_look_*"}.Detect(r.ctx)
	run.proposeFacts(r.ctx, props)
	if got := r.sender.cards(); len(got) != 2 {
		t.Fatalf("the first look's fact sent %d cards in all (%v), want 2", len(got), got)
	}
}

// Error propagation: a dispatcher failure is logged with the database and
// the fact, and the proposal stands.
func TestFactCardDispatchFailureKeepsTheProposal(t *testing.T) {
	r := newCardRig(t, &failingRules{})
	var f facts.Fact
	out := captureStderr(t, func() { f = r.proposeOverMCP(t, "test_fail_*") })
	if !strings.Contains(out, `db "orders": fact card `) ||
		!strings.Contains(out, "connection refused") {
		t.Fatalf("dispatch failure not logged with its cause: %q", out)
	}
	got, err := r.rt.facts.Get(r.ctx, f.ID)
	if err != nil || got.Status != facts.StatusProposed {
		t.Fatalf("fact after a failed card: %+v %v", got, err)
	}
}

// State transition: once the runtime stops, its pool sends no more cards.
func TestFactCardStopsWithTheRuntime(t *testing.T) {
	r := newCardRig(t, &cardRules{})
	r.rt.cancel()
	_, created, err := facts.NewStore(r.pool).Propose(r.ctx, facts.Proposal{
		Type: facts.TypeTestFixture, Kind: facts.KindSchema, Subject: "test_after_*",
		Source: facts.SourceOperator, ProposedBy: "alice@example.com"})
	if err != nil || !created {
		t.Fatalf("propose after stop: created=%v err=%v", created, err)
	}
	if got := r.sender.cards(); len(got) != 0 {
		t.Fatalf("a stopped runtime sent cards %v", got)
	}
}

// Concurrent access: MCP and mark_object proposing the same new fact at
// once send one card.
func TestFactCardConcurrentProposalsSendOneCard(t *testing.T) {
	r := newCardRig(t, &cardRules{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.mcp.ProposeFact(r.ctx, mcp.FactRequest{Database: "orders",
				Type: "owned_by_app_migrations", SubjectKind: "schema", Subject: "app_race_*",
				Evidence: "migrations"}, "agent:claude-code")
			if err != nil {
				t.Errorf("concurrent propose: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := r.sender.cards(); len(got) != 1 {
		t.Fatalf("six concurrent proposals sent %d cards, want 1", len(got))
	}
}
