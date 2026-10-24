package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// errChangeReviewNotRunning: the database has no Change Review service
// (it is not monitored here, or its runtime has not started it). Its
// message is safe to show the caller.
var errChangeReviewNotRunning = fmt.Errorf("%w: Change Review is not running",
	changereview.ErrUnavailable)

// NewChangeReviewBackend serves the Change Review tools from the
// process-wide registry each database runtime registers its service in.
func NewChangeReviewBackend(reg *changereview.Registry) ChangeReviewBackend {
	return changeReviewRegistry{reg: reg}
}

type changeReviewRegistry struct{ reg *changereview.Registry }

// service is the service of database; with no name, the only registered
// database's.
func (b changeReviewRegistry) service(database string) (*changereview.Service, error) {
	if database == "" {
		names := b.reg.Databases()
		switch len(names) {
		case 0:
			return nil, fmt.Errorf("%w on any database", errChangeReviewNotRunning)
		case 1:
			database = names[0]
		default:
			return nil, fmt.Errorf("%w: database is required: name one of %s",
				changereview.ErrInvalid, strings.Join(names, ", "))
		}
	}
	svc, err := b.reg.Service(database)
	if errors.Is(err, changereview.ErrNotFound) {
		return nil, fmt.Errorf("%w for database %q", errChangeReviewNotRunning, database)
	}
	return svc, err
}

func (b changeReviewRegistry) ReviewChange(ctx context.Context, database string,
	c changereview.Caller, r changereview.Request) (changereview.Review, bool, error) {
	svc, err := b.service(database)
	if err != nil {
		return changereview.Review{}, false, err
	}
	if r.Database == "" {
		r.Database = svc.Database()
	}
	return svc.Submit(ctx, c, r)
}

func (b changeReviewRegistry) GetChangeReview(ctx context.Context, database, id string,
	wait time.Duration) (changereview.Review, error) {
	svc, err := b.service(database)
	if err != nil {
		return changereview.Review{}, err
	}
	return svc.Wait(ctx, id, wait)
}

func (b changeReviewRegistry) ListChangeReviews(ctx context.Context, database string,
	f changereview.ListFilter) ([]changereview.Review, error) {
	svc, err := b.service(database)
	if err != nil {
		return nil, err
	}
	return svc.List(ctx, f)
}

func (b changeReviewRegistry) ReportChangeDeployed(ctx context.Context, database string,
	c changereview.Caller, d changereview.Deployment) (changereview.Review, error) {
	svc, err := b.service(database)
	if err != nil {
		return changereview.Review{}, err
	}
	return svc.ReportDeployed(ctx, c, d)
}

// ReviewChange serves review_change through ProductionDependencies.ChangeReview.
func (backend *ProductionBackend) ReviewChange(ctx context.Context, database string,
	c changereview.Caller, r changereview.Request) (changereview.Review, bool, error) {
	if backend.dependencies.ChangeReview == nil {
		return changereview.Review{}, false, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.ChangeReview.ReviewChange(ctx, database, c, r)
}

// GetChangeReview serves get_change_review.
func (backend *ProductionBackend) GetChangeReview(ctx context.Context, database, id string,
	wait time.Duration) (changereview.Review, error) {
	if backend.dependencies.ChangeReview == nil {
		return changereview.Review{}, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.ChangeReview.GetChangeReview(ctx, database, id, wait)
}

// ListChangeReviews serves list_change_reviews.
func (backend *ProductionBackend) ListChangeReviews(ctx context.Context, database string,
	f changereview.ListFilter) ([]changereview.Review, error) {
	if backend.dependencies.ChangeReview == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.ChangeReview.ListChangeReviews(ctx, database, f)
}

// ReportChangeDeployed serves report_change_deployed.
func (backend *ProductionBackend) ReportChangeDeployed(ctx context.Context, database string,
	c changereview.Caller, d changereview.Deployment) (changereview.Review, error) {
	if backend.dependencies.ChangeReview == nil {
		return changereview.Review{}, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.ChangeReview.ReportChangeDeployed(ctx, database, c, d)
}
