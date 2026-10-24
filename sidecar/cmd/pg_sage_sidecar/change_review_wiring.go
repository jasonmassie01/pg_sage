package main

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/changereview/reasoner"
	"github.com/pg-sage/sidecar/internal/changereview/receipt"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Change Review wiring. Each database runtime builds its own service: the
// impact engine and the reasoner read only that database's pool, reviews
// are stored in its own sage.change_review, and the runner's goroutines
// run on the runtime's worker group. The service is registered in one
// process-wide registry (change_review_registry.go) that the REST routes and the MCP tools route
// through, and removed when the runtime stops. The reasoner always runs:
// without a model it keeps reviews deterministic (model_status no_model)
// and gives ORM-only migrations CR-20.

// startChangeReview registers the database's Change Review until the
// runtime stops.
func (rt *databaseRuntime) startChangeReview() {
	svc, err := rt.newChangeReview()
	if err == nil {
		_, err = svc.Start(rt.ctx)
	}
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: Change Review not started: %v", rt.spec.Name, err)
		return
	}
	changeReviewServices().Set(rt.spec.Name, svc)
	rt.start(func() {
		<-rt.ctx.Done()
		changeReviewServices().Remove(rt.spec.Name, svc)
	})
	rt.note("change_review")
}

// newChangeReview builds the service on the runtime's pool, model and
// dispatcher.
func (rt *databaseRuntime) newChangeReview() (*changereview.Service, error) {
	return buildChangeReview(changeReviewParts{Database: rt.spec.Name, Pool: rt.spec.Pool,
		Config: rt.cfg, Model: changeReviewModel(rt.llmManager), Dispatcher: rt.dispatcher,
		Go: rt.start,
		// The stages log only what failed or degraded.
		Logf: func(format string, args ...any) { logWarn(rt.spec.Scope, format, args...) }})
}

// changeReviewParts is what one database's Change Review is built from.
type changeReviewParts struct {
	Database   string
	Pool       *pgxpool.Pool
	Config     *config.Config
	Model      agentloop.Model // nil: no model (deterministic reviews, CR-20)
	Dispatcher *notify.Dispatcher
	Go         func(func())
	Logf       func(format string, args ...any)
}

// buildChangeReview composes the production service: the impact engine
// as analyzer and inspector, the reasoner over the budgeted model, the
// needs-DBA notifier, diffscan's hunk filter and redactor, and the shadow
// auto-clear rule.
func buildChangeReview(p changeReviewParts) (*changereview.Service, error) {
	crCfg := p.Config.ChangeReview
	engine, err := impact.New(impact.Deps{Pool: p.Pool, Config: crCfg,
		Timeouts: catalogread.FromSafety(p.Config.Safety), Logf: p.Logf})
	if err != nil {
		return nil, fmt.Errorf("impact engine: %w", err)
	}
	budget := changereview.NewBudget(p.Pool, int64(crCfg.DailyTokensPerDatabase),
		int64(crCfg.DailyTokensPerActor), nil)
	r, err := reasoner.New(reasoner.Deps{Model: p.Model, Tools: engine, Budget: budget,
		Logf: p.Logf})
	if err != nil {
		return nil, fmt.Errorf("reasoner: %w", err)
	}
	render := changeReviewRender(p.Config)
	return changereview.New(changereview.Deps{Database: p.Database, Pool: p.Pool,
		Config: crCfg, Analyzer: engine, Reasoner: r,
		Notifier:   newChangeReviewNotifier(p.Dispatcher, render, p.Logf),
		HunkFilter: diffscan.HunkFilter, Redact: diffscan.Redact, Render: render,
		AutoClear: receipt.AutoClear(crCfg), Go: p.Go, Logf: p.Logf})
}

// changeReviewRender is how reviews link the UI: through
// change_review.ui_base_url (empty: they name the review instead).
func changeReviewRender(cfg *config.Config) changereview.RenderOptions {
	return changereview.RenderOptions{UIBaseURL: cfg.ChangeReview.UIBaseURL}
}

// changeReviewModel is the client for the change_review purpose (the
// general one), or no model. The reasoner checks per review whether the
// client is switched on, so a hot-reloaded llm.enabled takes effect.
func changeReviewModel(m *llm.Manager) agentloop.Model {
	if m == nil {
		return nil
	}
	if c := m.ForPurpose(reasoner.Purpose); c != nil {
		return c
	}
	return nil
}
