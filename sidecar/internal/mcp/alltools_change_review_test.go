package mcp

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// allToolsBackend serves the Change Review tools too.

func crAllToolsReview(database string) changereview.Review {
	return changereview.Review{ID: "5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11", Database: database,
		State: changereview.StateComplete}
}

func (b *allToolsBackend) ReviewChange(ctx context.Context, database string,
	_ changereview.Caller, _ changereview.Request) (changereview.Review, bool, error) {
	b.hit(ctx, "review_change")
	return crAllToolsReview(database), true, nil
}

func (b *allToolsBackend) GetChangeReview(ctx context.Context, database, _ string,
	_ time.Duration) (changereview.Review, error) {
	b.hit(ctx, "get_change_review")
	return crAllToolsReview(database), nil
}

func (b *allToolsBackend) ListChangeReviews(ctx context.Context, database string,
	_ changereview.ListFilter) ([]changereview.Review, error) {
	b.hit(ctx, "list_change_reviews")
	return []changereview.Review{crAllToolsReview(database)}, nil
}

func (b *allToolsBackend) ReportChangeDeployed(ctx context.Context, database string,
	_ changereview.Caller, _ changereview.Deployment) (changereview.Review, error) {
	b.hit(ctx, "report_change_deployed")
	return crAllToolsReview(database), nil
}
