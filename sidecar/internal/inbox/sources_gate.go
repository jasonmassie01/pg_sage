package inbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/pg-sage/sidecar/internal/review/modelgate"
)

// ModelGatePromotions lists a database's live G-M1 promotion (review-05a):
// the model gate passed for a review model, and only an admin's approval
// lets that model's review steps run at their class's trust level.
func ModelGatePromotions(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		if in == nil {
			return nil, nil // no fleet: no review database to read
		}
		pool, err := poolOf(in, db)
		if errors.Is(err, ErrNotConnected) {
			// The ledger's promotions still list offline; the other kinds
			// already report this database as not connected.
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		rows, err := modelgate.New(pool, db, modelgate.Options{}).Pending(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(rows))
		for _, r := range rows {
			out = append(out, modelGateItem(db, r))
		}
		return out, nil
	})
}

// modelGateItem is the promotion of row r of database db. It is high risk:
// once granted, the model's steps no longer need their own approval.
func modelGateItem(db string, r modelgate.Row) Item {
	id := strconv.FormatInt(r.ID, 10)
	return Item{Kind: KindAutonomyPromotion, ID: "review-gate-" + id, Database: db,
		Title: fmt.Sprintf("Let model %s's review steps run at their class's trust level "+
			"(G-M1)", r.Model),
		Why:  r.Reason,
		Risk: RiskHigh, CreatedAt: r.EvaluatedAt,
		Href:   href("trust", "database", db, "review_gate", id),
		Source: "sage.review_gate"}
}

// allOf lists every source's items for one kind; the first error fails the
// kind, so a source that could not be read is never shown as empty.
func allOf(sources ...Source) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		var out []Item
		for _, s := range sources {
			items, err := s.List(ctx, db)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	})
}
