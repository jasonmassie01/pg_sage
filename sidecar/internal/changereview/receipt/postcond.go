package receipt

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
)

// evaluateAll evaluates the post-conditions of every review in one
// bounded catalog statement (impact.EvaluatePostConditions over their
// concatenation; results come back in input order) and returns each
// review's landing (impact.Landing). When the statement refuses a
// malformed post-condition, each review is evaluated on its own so only
// the malformed ones fail; any other failure fails them all.
func evaluateAll(ctx context.Context, q catalogread.Querier,
	reviews []changereview.Review) (map[string]string, map[string]error) {
	states, failed := map[string]string{}, map[string]error{}
	var all []changereview.PostCondition
	var owners []string
	for _, r := range reviews {
		for _, pc := range r.Expectations.PostConditions {
			all = append(all, pc)
			owners = append(owners, r.ID)
		}
	}
	if len(all) == 0 {
		return states, failed
	}
	res, err := impact.EvaluatePostConditions(ctx, q, all)
	switch {
	case errors.Is(err, changereview.ErrInvalid):
		return evaluateEach(ctx, q, reviews)
	case err != nil:
		for _, id := range owners {
			failed[id] = err
		}
		return states, failed
	}
	per := map[string][]impact.PostResult{}
	for i, pr := range res {
		per[owners[i]] = append(per[owners[i]], pr)
	}
	for id, prs := range per {
		states[id] = impact.Landing(prs)
	}
	return states, failed
}

// evaluateEach evaluates each review's post-conditions on its own.
func evaluateEach(ctx context.Context, q catalogread.Querier,
	reviews []changereview.Review) (map[string]string, map[string]error) {
	states, failed := map[string]string{}, map[string]error{}
	for _, r := range reviews {
		if len(r.Expectations.PostConditions) == 0 {
			continue
		}
		res, err := impact.EvaluatePostConditions(ctx, q, r.Expectations.PostConditions)
		if err != nil {
			failed[r.ID] = err
			continue
		}
		states[r.ID] = impact.Landing(res)
	}
	return states, failed
}
