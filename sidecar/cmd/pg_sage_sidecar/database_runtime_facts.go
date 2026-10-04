package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Binding facts (roadmap 2.3): wiring. Each database has one fact store
// (sage.facts in the monitored database). The policy gate binds confirmed
// facts on every authorization; the analyzer removes confirmed test
// fixtures and redirects bound changes to source-fix packets; the
// optimizer, advisor and investigator prompts carry confirmed facts; a
// worker imports earlier declarations, runs the detectors (and the model,
// when an LLM is configured), sends a fact card for every new proposal
// and re-verifies facts against the catalog.

// Detector and card settings.
const (
	factFixtureQuiet     = 6 * time.Hour
	factAppendOnlyMin    = 1_000_000
	factCardTTL          = 7 * 24 * time.Hour
	factEvidenceInCard   = 3
	factCardDetailLength = 300
)

// newFactStore is the fact store of a monitored database.
func newFactStore(pool *pgxpool.Pool) *facts.Store {
	return facts.NewStore(pool).WithLog(logStructuredWrapper)
}

// factBinder is the gate's typed view of the database's confirmed facts.
func (rt *databaseRuntime) factBinder() *facts.Binder {
	return facts.NewBinder(rt.facts, facts.NewCatalogResolver(rt.spec.Pool), nil)
}

// factFilter applies the confirmed facts to the analyzer's cycles.
func (rt *databaseRuntime) factFilter() *facts.FindingFilter {
	return facts.NewFindingFilter(rt.facts, logStructuredWrapper)
}

// startFacts runs the database's facts worker on the instance worker group.
func (rt *databaseRuntime) startFacts() {
	pool := rt.spec.Pool
	opts := facts.WorkerOptions{
		Detectors: []facts.Detector{
			facts.NewAppManagedDetector(pool),
			facts.NewTestFixtureDetector(pool, factFixtureQuiet, nil),
			facts.NewSlotConsumerDetector(pool),
			facts.NewAppendOnlyDetector(pool, factAppendOnlyMin),
		},
		Notify: rt.notifyFact,
		LogFn:  logStructuredWrapper,
	}
	if rt.llmOn {
		opts.Model = facts.NewModelProposer(rt.generalLLM).WithLog(logStructuredWrapper)
		opts.ModelEvidence = func(ctx context.Context) ([]facts.EvidenceItem, error) {
			return facts.CollectModelEvidence(ctx, pool)
		}
	}
	w := facts.NewWorker(rt.facts, opts)
	rt.start(func() { w.Run(rt.ctx) })
	rt.note("facts")
}

// notifyFact sends the card of a new proposal to the approval channels.
func (rt *databaseRuntime) notifyFact(ctx context.Context, f facts.Fact) {
	if rt.dispatcher == nil {
		return
	}
	ref := notify.FactCardRef{Database: rt.spec.Name, FactID: f.ID, FactHash: f.Hash(),
		Title: f.Describe(), ExpiresAt: time.Now().Add(factCardTTL)}
	if err := rt.dispatcher.Dispatch(ctx, notify.FactProposedEvent(ref,
		factCardBody(f))); err != nil {
		logWarn(rt.spec.Scope, "db %q: fact card %d not sent: %v", rt.spec.Name, f.ID, err)
	}
}

// factCardBody is the card text: who proposed it, the cited evidence and
// what confirming does.
func factCardBody(f facts.Fact) string {
	lines := []string{fmt.Sprintf("Database fact #%d proposed by %s (%s).", f.ID, f.Source,
		f.ProposedBy)}
	if f.Rationale != "" {
		lines = append(lines, "Why: "+f.Rationale)
	}
	for i, c := range f.Evidence {
		if i == factEvidenceInCard {
			break
		}
		lines = append(lines, "Evidence: "+truncateText(c.Ref+": "+c.Detail,
			factCardDetailLength))
	}
	lines = append(lines, "Confirming only narrows what pg_sage does on its own; "+
		"nothing changes until a person confirms.")
	return strings.Join(lines, "\n")
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// factCardIssuer mints fact card tokens in the notification control
// database, where the chat callbacks look them up.
type factCardIssuer struct{ store *chatops.FactCardStore }

func (i factCardIssuer) Issue(ctx context.Context, ch notify.Channel,
	ref notify.FactCardRef) (string, error) {
	return i.store.Issue(ctx, chatops.FactCardIssue{ChannelID: ch.ID,
		Database: ref.Database, FactID: ref.FactID, FactHash: ref.FactHash,
		Title: ref.Title, ExpiresAt: ref.ExpiresAt})
}

func (i factCardIssuer) Revoke(ctx context.Context, token string) error {
	return i.store.Revoke(ctx, token)
}

// withFactCardTokens makes a control pool's dispatcher mint fact tokens.
func withFactCardTokens(d *notify.Dispatcher, controlPool *pgxpool.Pool) {
	if d == nil || controlPool == nil {
		return
	}
	d.WithFactTokens(factCardIssuer{store: chatops.NewFactCardStore(controlPool)})
}
