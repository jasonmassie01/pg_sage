package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// CensusLister reads the standing TTL proposals of the newest sprawl
// census (*fleet.CensusDB, through the fleet manager).
type CensusLister interface {
	CensusProposals(ctx context.Context) ([]fleet.CensusProposal, error)
}

// CensusAdvice lists the sprawl census's TTL proposals (fleetpol-03) as
// advice, fleet-wide (FleetWide): an observe-only member has no runtime,
// so no per-database source can read it. A fleet without a census has no
// advice to list. Nothing here deletes or schedules anything: a person
// sets a TTL on the member's page, or adopts it.
func CensusAdvice(l CensusLister) Source {
	return FleetWide(func(ctx context.Context, _ string) ([]Item, error) {
		if l == nil {
			return nil, nil
		}
		got, err := l.CensusProposals(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(got))
		for _, p := range got {
			out = append(out, censusItem(p))
		}
		return out, nil
	})
}

// censusOf is the census a fleet's instances read through, if any.
func censusOf(in Instances) CensusLister {
	if l, ok := in.(CensusLister); ok {
		return l
	}
	return nil
}

func censusItem(p fleet.CensusProposal) Item {
	finding, _, _ := strings.Cut(p.Proposal, ":")
	why := fmt.Sprintf("sprawl census of %s: %s. Deleting is never pg_sage's: a person "+
		"decides (set a TTL, or adopt the database)", p.RunAt.Format("2006-01"), p.Proposal)
	if p.Owner != "" {
		why += "; owner " + p.Owner
	}
	if p.Creator != "" {
		why += "; created by " + p.Creator
	}
	return Item{Kind: KindAdvice, ID: fmt.Sprintf("census-%d-%s", p.RunID, p.DatabaseKey),
		Database: p.DatabaseKey, Title: "Set a TTL on " + p.DatabaseKey + ": " + finding,
		Why: why, Risk: RiskSafe, CreatedAt: p.RunAt,
		Href:   href("manage-databases", "database", p.DatabaseKey),
		Source: "sage.sprawl_census"}
}
