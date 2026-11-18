package inbox

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Promotions lists earned-autonomy promotions waiting for an admin. A
// database without a ledger (none registered, or autonomy off) has no
// promotion to decide, so it lists none rather than failing.
func Promotions(reg *earned.Registry) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		if reg == nil {
			return nil, nil
		}
		entry, ok := reg.Lookup(db)
		if !ok || entry.Service == nil {
			return nil, nil
		}
		got, err := entry.Service.OpenProposals(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(got))
		for _, p := range got {
			due, risk := p.ExpiresAt, RiskModerate
			if p.To >= earned.L3 {
				risk = RiskHigh // L3 runs unattended
			}
			out = append(out, Item{Kind: KindAutonomyPromotion, ID: p.ID, Database: db,
				Title: fmt.Sprintf("Promote %s/%s from %s to %s", p.Family, p.Class,
					p.From, p.To),
				Why:  "the trust ledger's evidence supports a promotion; an admin decides",
				Risk: risk, CreatedAt: p.ProposedAt, DueAt: &due,
				Href:   href("trust", "database", db, "proposal", p.ID),
				Source: "sage.sre_autonomy_proposals"})
		}
		return out, nil
	})
}

// Runbooks lists runbooks whose latest version is an unsigned draft. A
// database without the SRE investigator has no runbook to sign (its
// runbook routes answer 503), so it lists none.
func Runbooks(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		inst, err := instanceOf(in, db)
		if err != nil {
			return nil, err
		}
		if inst.Investigations == nil {
			return nil, nil
		}
		got, err := inst.Investigations.Runbooks(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(got))
		for _, rb := range got {
			if rb.Status != sre.RunbookDraft {
				continue
			}
			id := string(rb.ID)
			out = append(out, Item{Kind: KindRunbookSignature, ID: id, Database: db,
				Title: fmt.Sprintf("Sign runbook %q version %d", rb.Latest.Name,
					rb.LatestVersion),
				Why:  "an unsigned version never runs; an admin signs its content hash",
				Risk: RiskHigh, CreatedAt: rb.Latest.CreatedAt,
				Href:   href("advanced/runbooks", "database", db, "runbook", id),
				Source: "sage.sre_runbook_versions"})
		}
		return out, nil
	})
}

// FleetSources is every kind's source over a fleet (or a read view of
// it), the trust ledgers and, when the fleet has one, its sprawl census.
// Autonomy promotions are the ledgers' and the review model gate's
// (review-05a).
func FleetSources(in Instances, ledgers *earned.Registry) map[Kind]Source {
	return map[Kind]Source{
		KindChangeApproval:    ChangeApprovals(in),
		KindPlanApproval:      Plans(in),
		KindAutonomyPromotion: allOf(Promotions(ledgers), ModelGatePromotions(in)),
		KindFact:              Facts(in),
		KindRunbookSignature:  Runbooks(in),
		KindAgentRequest:      AgentRequests(in),
		KindPolicyException:   joinSources(PolicyProposals(in), PolicyExceptions(in)),
		KindAdvice:            CensusAdvice(censusOf(in)),
	}
}
