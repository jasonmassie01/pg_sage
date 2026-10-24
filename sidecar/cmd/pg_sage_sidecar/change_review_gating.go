package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The review that gates a pull request's status in the scheduled job.
// pg_sage cannot tell the CI run's review from another caller's (any
// read-scoped token or viewer may submit one with the PR's URL), so the
// job never trusts "the newest": it takes the reviews of the head commit
// (a review naming no head commit gates nothing) and, of those, the most
// restrictive gate. A planted review can hold the status back, never turn
// a failing one green. Complete reviews gate first; a running one only
// when none has completed, a failed one only when nothing else exists (a
// transient failure followed by a good run does not block the PR).

// gatingReview is the review whose gate the head commit of prURL gets.
func (c *reviewCLI) gatingReview(ctx context.Context, prURL, head string) (fetched, bool,
	error) {
	if head == "" {
		return fetched{}, false, nil
	}
	obj, err := c.client.call(ctx, "list_change_reviews", map[string]any{
		"database": c.o.database, "pr_url": prURL, "limit": changereview.MaxListLimit})
	if err != nil {
		return fetched{}, false, err
	}
	items, _ := obj["reviews"].([]any)
	var atHead []fetched
	for _, item := range items {
		raw, _ := item.(map[string]any)
		f, err := decodeFetched(raw)
		if err != nil {
			return fetched{}, false, err
		}
		if f.review.HeadSHA == head {
			atHead = append(atHead, f)
		}
	}
	best, ok := mostRestrictive(atHead, c.o.failOn)
	if !ok {
		return fetched{}, false, nil
	}
	f, err := c.getReview(ctx, best.review.ID)
	return f, err == nil, err
}

// stateOrder is which reviews gate: complete first, then running, then
// failed.
var stateOrder = map[string]int{changereview.StateComplete: 0,
	changereview.StateRunning: 1, changereview.StateFailed: 2}

// gateRank orders gates from the most restrictive: a failure, an error,
// a pending check, then a pass.
var gateRank = map[string]int{"failure": 0, "error": 1, "pending": 2, "success": 3}

// mostRestrictive is, of the reviews in the best state present, the one
// whose gate is most restrictive (the newest of equals: listings come
// newest first).
func mostRestrictive(reviews []fetched, failOn string) (fetched, bool) {
	var best fetched
	found := false
	for _, f := range reviews {
		if !found || better(f, best, failOn) {
			best, found = f, true
		}
	}
	return best, found
}

// better reports whether f gates before cur.
func better(f, cur fetched, failOn string) bool {
	fs, cs := stateOrder[f.review.State], stateOrder[cur.review.State]
	if fs != cs {
		return fs < cs
	}
	fg, cg := gateOf(f.review, failOn), gateOf(cur.review, failOn)
	if fg.pass != cg.pass {
		return !fg.pass
	}
	return gateRank[fg.state] < gateRank[cg.state]
}
