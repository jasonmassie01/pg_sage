package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Plan cards (plan-03b). Every plan stored as proposed on the database's
// pool, whoever proposed it (an agent over MCP today), sends its card to
// the channels that take approval cards, with the buttons its risk tiers
// allow from chat (decision D7). No new configuration: a channel without
// interactive callbacks gets the card without buttons.

// planCardTTL is how long a plan card's buttons work.
const planCardTTL = 7 * 24 * time.Hour

// registerPlanCards makes every plan proposed on the database's pool send
// its card. The registration ends with the runtime.
func (rt *databaseRuntime) registerPlanCards() {
	unset := changeplan.SetProposalNotifier(rt.spec.Pool, func(ctx context.Context,
		p changeplan.Plan) {
		if rt.ctx.Err() == nil {
			rt.notifyPlan(ctx, p)
		}
	})
	context.AfterFunc(rt.ctx, unset)
}

// notifyPlan sends the card of a newly proposed plan. A failure is logged:
// the plan can still be decided in the app.
func (rt *databaseRuntime) notifyPlan(ctx context.Context, p changeplan.Plan) {
	if rt.dispatcher == nil {
		return
	}
	d, err := changeplan.NewStore(rt.spec.Pool).Detail(ctx, p.ID)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: plan card %d not sent: %v", rt.spec.Name, p.ID, err)
		return
	}
	ref := planCardRef(rt.spec.Name, d, time.Now().Add(planCardTTL))
	if err := rt.dispatcher.Dispatch(ctx, notify.PlanProposedEvent(ref,
		planCardBody(rt.spec.Name, d))); err != nil {
		logWarn(rt.spec.Scope, "db %q: plan card %d not sent: %v", rt.spec.Name, p.ID, err)
	}
}

// planCardRef is the card of plan detail d: the decisions chat may make.
func planCardRef(database string, d changeplan.Detail, expires time.Time) notify.PlanCardRef {
	offer := changeplan.OfferForChat(d)
	ref := notify.PlanCardRef{Database: database, PlanID: d.Plan.ID,
		PlanHash: changeplan.PlanHash(d.Plan), Title: d.Plan.Goal, Steps: len(d.Plan.Steps),
		ExpiresAt: expires}
	if offer.All {
		ref.Offered = append(ref.Offered, chatops.PlanApproveAll)
	}
	if offer.UpTo > 0 {
		ref.Offered, ref.UpTo = append(ref.Offered, chatops.PlanApproveUpTo), offer.UpTo
	}
	if offer.Next {
		ref.Offered = append(ref.Offered, chatops.PlanApproveNext)
	}
	ref.Offered = append(ref.Offered, chatops.PlanReject)
	return ref
}

// planCardBody is the card text: goal, step count, each step's expected
// effect and risk, what chat may approve and where to review the plan.
func planCardBody(database string, d changeplan.Detail) string {
	p := d.Plan
	lines := []string{fmt.Sprintf("Plan #%d on %s, proposed by %s (%s): %d steps.", p.ID,
		database, p.CreatedBy, p.Origin, len(p.Steps)),
		"Goal: " + truncateText(p.Goal, factCardDetailLength)}
	highest := policy.RiskSafe
	for _, s := range p.Steps {
		tier := changeplan.StepRiskTier(s)
		if tierAbove(tier, highest) {
			highest = tier
		}
		lines = append(lines, fmt.Sprintf("Step %d (%s, risk %s): %s", s.Ordinal, s.Kind,
			tier, stepEffect(s)))
	}
	lines = append(lines, "Risk: highest step tier "+string(highest)+".")
	if offer := changeplan.OfferForChat(d); offer.BlockedAt > 0 {
		lines = append(lines, fmt.Sprintf("From chat: %s, so step %d onward needs the "+
			"pg_sage app.", offer.Blocked, offer.BlockedAt))
	}
	lines = append(lines, fmt.Sprintf("Review it in the pg_sage app (Plans), or run: "+
		"pg_sage_sidecar plan show %d --database %s", p.ID, database))
	return strings.Join(lines, "\n")
}

func tierAbove(a, b policy.RiskTier) bool {
	rank := map[policy.RiskTier]int{policy.RiskSafe: 1, policy.RiskModerate: 2,
		policy.RiskHigh: 3}
	ra, ok := rank[a]
	if !ok {
		ra = 4
	}
	return ra > rank[b]
}

// stepEffect is what a step is expected to do to its metric.
func stepEffect(s changeplan.Step) string {
	if s.Metric == changeplan.MetricReadback {
		return "sets " + s.Setting + " and reads it back"
	}
	effect := fmt.Sprintf("%s expected %s", s.Metric, s.ExpectedDirection)
	if s.ExpectedPct != nil {
		effect += fmt.Sprintf(" by about %.0f%% (%s)", *s.ExpectedPct, s.ExpectedSource)
	}
	return effect
}

// planCardIssuer mints plan card tokens in the notification control
// database, where the chat callbacks look them up.
type planCardIssuer struct{ store *chatops.PlanCardStore }

func (i planCardIssuer) Issue(ctx context.Context, ch notify.Channel,
	ref notify.PlanCardRef) (string, error) {
	return i.store.Issue(ctx, chatops.PlanCardIssue{ChannelID: ch.ID,
		Database: ref.Database, PlanID: ref.PlanID, PlanHash: ref.PlanHash,
		Title: ref.Title, Offered: ref.Offered, UpTo: ref.UpTo, ExpiresAt: ref.ExpiresAt})
}

func (i planCardIssuer) Revoke(ctx context.Context, token string) error {
	return i.store.Revoke(ctx, token)
}

// withPlanCardTokens makes a control pool's dispatcher mint plan tokens.
func withPlanCardTokens(d *notify.Dispatcher, controlPool *pgxpool.Pool) {
	if d == nil || controlPool == nil {
		return
	}
	d.WithPlanTokens(planCardIssuer{store: chatops.NewPlanCardStore(controlPool)})
}
