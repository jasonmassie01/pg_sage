package api

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

// enrichFindingCases attaches the action timeline, the recommendation
// head and the provider advice of each of one database's finding cases.
func enrichFindingCases(
	ctx context.Context, projected []cases.Case, selected namedPool, findingIDs []int,
) {
	enrichCaseActionTimelines(ctx, projected, selected,
		store.NewActionStore(selected.pool), findingIDs)
	enrichCaseRecommendations(ctx, projected, selected, findingIDs, time.Now().UTC())
	enrichCaseProviderAdvice(ctx, projected, selected, findingIDs)
	enrichCasePlans(ctx, projected, selected, findingIDs)
}

// enrichCaseRecommendations attaches each finding case's newest
// recommendation head, read in one query for the database (cases-10). A
// failed read is logged and marks every finding case
// recommendation_unknown, never "no recommendation".
func enrichCaseRecommendations(
	ctx context.Context, projected []cases.Case, selected namedPool,
	findingIDs []int, now time.Time,
) {
	if len(findingIDs) == 0 {
		return
	}
	ids := make([]int64, len(findingIDs))
	for i, id := range findingIDs {
		ids[i] = int64(id)
	}
	var q recommendation.Querier
	if selected.pool != nil {
		q = selected.pool
	}
	heads, err := recommendation.LatestByFindingIDs(ctx, q, ids)
	for i := range projected {
		findingID, ok := caseFindingID(projected[i])
		if !ok {
			continue
		}
		if err != nil {
			cases.MarkRecommendationUnknown(&projected[i])
			continue
		}
		if head, found := heads[int64(findingID)]; found {
			cases.AttachRecommendation(&projected[i], head,
				recommendationURL(head.ID, selected.name), now)
		}
	}
	if err != nil {
		slog.Warn("case recommendation read failed; marked recommendation_unknown",
			"database", selected.name, "findings", len(ids), "error", err)
	}
}

// recommendationURL is the read API of one head in database.
func recommendationURL(id int64, database string) string {
	return "/api/v1/recommendations/" + strconv.FormatInt(id, 10) +
		"?database=" + url.QueryEscape(database)
}
