package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Binding facts (roadmap 2.3): wiring. Each database has one fact store
// (sage.facts in the monitored database). The policy gate binds confirmed
// facts on every authorization; the analyzer removes confirmed test
// fixtures and redirects bound changes to source-fix packets; the
// optimizer, advisor and investigator prompts carry confirmed facts; a
// worker imports earlier declarations, runs the detectors (and the model,
// when an LLM is configured) and re-verifies facts against the catalog;
// every new proposal on the pool, whoever made it, sends a fact card.

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

// initFacts opens the database's fact store and joins the process's
// cluster registry for the life of the runtime, so its siblings' gates
// see its slot facts (facts-02).
func (rt *databaseRuntime) initFacts() {
	rt.facts = newFactStore(rt.spec.Pool).WithConfirmedAbsence(
		time.Duration(factAbsenceDays(rt.cfg)) * 24 * time.Hour)
	rt.factCluster = processFactClusters.join(rt.ctx,
		factClusterIdentity(rt.ctx, rt.spec.Pool), rt.spec.Name, rt.facts)
}

// factBinder is the gate's typed view of the database's confirmed facts and
// of the slot facts of its sibling databases on the same cluster.
func (rt *databaseRuntime) factBinder() *facts.Binder {
	return newClusterFactBinder(rt.spec.Pool, rt.facts, processFactClusters, rt.factCluster)
}

// factFilter applies the confirmed facts to the analyzer's cycles, with
// refs resolved against the catalog (a fact on a partitioned table
// redirects findings on its partitions); its cleanup batches embed
// activity baselines read from the database.
func (rt *databaseRuntime) factFilter() *facts.FindingFilter {
	return facts.NewFindingFilter(rt.facts, catalogResolver(rt.spec.Pool),
		logStructuredWrapper).WithActivity(facts.NewActivityReader(rt.spec.Pool))
}

// startFacts runs the database's facts worker on the instance worker group.
func (rt *databaseRuntime) startFacts() {
	w := facts.NewWorker(rt.facts, rt.factWorkerOptions())
	rt.start(func() { w.Run(rt.ctx) })
	rt.note("facts")
}

// factWorkerOptions are the worker's detectors, model and expiry notice;
// its proposals' cards go out through the pool's notifier
// (registerFactCards).
func (rt *databaseRuntime) factWorkerOptions() facts.WorkerOptions {
	pool := rt.spec.Pool
	opts := facts.WorkerOptions{
		Detectors: []facts.Detector{
			facts.NewAppManagedDetector(pool),
			facts.NewTestFixtureDetector(pool, factFixtureQuiet, nil),
			facts.NewSlotConsumerDetector(pool),
			facts.NewAppendOnlyDetector(pool, factAppendOnlyMin),
		},
		NotifyExpired: rt.notifyFactExpired,
		LogFn:         logStructuredWrapper,
	}
	if rt.llmOn {
		opts.Model = facts.NewModelProposer(rt.generalLLM).WithLog(logStructuredWrapper)
		opts.ModelEvidence = func(ctx context.Context) ([]facts.EvidenceItem, error) {
			return facts.CollectModelEvidence(ctx, pool)
		}
	}
	return opts
}

// registerFactCards makes every new or reopened proposal on the database's
// pool send its fact card, whichever path proposed it: the worker, the
// first look, MCP propose_fact and mark_object, Ask Sage (facts-09). The
// registration ends with the runtime.
func (rt *databaseRuntime) registerFactCards() {
	unset := facts.SetProposalNotifier(rt.spec.Pool, func(ctx context.Context,
		f facts.Fact) {
		if rt.ctx.Err() == nil {
			rt.notifyFact(ctx, f)
		}
	})
	context.AfterFunc(rt.ctx, unset)
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

// factAbsenceDays is facts.confirmed_absence_days (its default without a
// config).
func factAbsenceDays(cfg *config.Config) int {
	if cfg == nil || cfg.Facts.ConfirmedAbsenceDays <= 0 {
		return config.DefaultFactsConfirmedAbsenceDays
	}
	return cfg.Facts.ConfirmedAbsenceDays
}

// notifyFactExpired tells the channels that a confirmed fact expired and
// no longer binds (facts-08). A proposal's expiry is nobody's loss.
func (rt *databaseRuntime) notifyFactExpired(ctx context.Context, f facts.Fact) {
	if rt.dispatcher == nil || !f.ConfirmationLost() {
		return
	}
	x := notify.FactExpiry{Database: rt.spec.Name, FactID: f.ID, Title: f.Describe(),
		ConfirmedBy: f.DecidedBy, Reason: f.ExpiredReason, MissingSince: f.MissingSince}
	if f.MissingSince != nil {
		x.RestoreDays = factAbsenceDays(rt.cfg)
	}
	if err := rt.dispatcher.Dispatch(ctx, notify.FactExpiredEvent(x)); err != nil {
		logWarn(rt.spec.Scope, "db %q: fact %d expiry not notified: %v", rt.spec.Name,
			f.ID, err)
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
