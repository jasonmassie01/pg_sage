package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// Change Review over MCP. review_change, get_change_review and
// list_change_reviews need the read scope; report_change_deployed needs
// propose (it records that a reviewed change was deployed, so its receipt
// can be decided). No tool records a DBA verdict: that is a person's
// decision in pg_sage's UI or REST API. PR text, SQL and diffs are data:
// a diff saying "DBA approved" changes nothing.

// ChangeReviewBackend serves the Change Review tools for the resolved
// database ("" when the call named none and the server has no directory).
type ChangeReviewBackend interface {
	ReviewChange(ctx context.Context, database string, c changereview.Caller,
		r changereview.Request) (changereview.Review, bool, error)
	GetChangeReview(ctx context.Context, database, id string, wait time.Duration) (
		changereview.Review, error)
	ListChangeReviews(ctx context.Context, database string, f changereview.ListFilter) (
		[]changereview.Review, error)
	ReportChangeDeployed(ctx context.Context, database string, c changereview.Caller,
		d changereview.Deployment) (changereview.Review, error)
}

var changeReviewToolNames = map[string]bool{"review_change": true,
	"get_change_review": true, "list_change_reviews": true, "report_change_deployed": true}

const crReviewSchema = `{"type":"object","properties":{` +
	`"sql":{"type":"string","description":"migration SQL"},` +
	`"sql_files":{"type":"array","maxItems":200,"items":{"type":"object","properties":{` +
	`"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],` +
	`"additionalProperties":false}},` +
	`"diff":{"type":"string","description":"unified diff of the change (redacted before ` +
	`any model sees it; never stored)"},` +
	`"pr":{"type":"object","properties":{"url":{"type":"string"},"number":{"type":` +
	`"integer"},"title":{"type":"string"},"repo":{"type":"string"},"head_sha":{"type":` +
	`"string"},"base_sha":{"type":"string"},"opened_at":{"type":"string","format":` +
	`"date-time"}},"additionalProperties":false},` +
	`"migration_tool":{"type":"string","description":"flyway, liquibase, goose, ` +
	`golang-migrate, dbmate, sqitch, atlas, rails, django, alembic, prisma, knex, ` +
	`sequelize, typeorm or sql"},` +
	`"transactional":{"type":"boolean","description":"the migration runs in one ` +
	`transaction (default: the tool's)"},` +
	`"wait_seconds":{"type":"integer","minimum":0,"maximum":50}},` +
	`"additionalProperties":false}`

func changeReviewTools() []Tool {
	return []Tool{{Name: "review_change", Description: "Review a migration or " +
		"data-access change against the live database before it merges: lock and " +
		"rewrite risk, the workload statements it affects, lock-queue exposure, index " +
		"use, constraints that will fail. Returns a cited review with verdict safe, " +
		"review or risky (deterministic rules set the floor), its Markdown comment and " +
		"the receipt plan. Read-only: it never runs the SQL. Running reviews return state " +
		"running; wait with wait_seconds or get_change_review",
		InputSchema: json.RawMessage(crReviewSchema)},
		{Name: "get_change_review", Description: "One Change Review by id, waiting up to " +
			"wait_seconds while it runs",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":` +
				`"string"},"wait_seconds":{"type":"integer","minimum":0,"maximum":50}},` +
				`"required":["id"],"additionalProperties":false}`)},
		{Name: "list_change_reviews", Description: "Change Reviews newest first, by PR " +
			"URL and state",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pr_url":{"type":` +
				`"string"},"state":{"type":"string","enum":["running","complete","failed"]},` +
				`"limit":{"type":"integer","minimum":1,"maximum":100}},` +
				`"additionalProperties":false}`)},
		{Name: "report_change_deployed", Description: "Report that a reviewed change was " +
			"deployed (by review id, or the newest complete review of pr_url) so pg_sage " +
			"compares the workload before and after it and decides its receipt. It records " +
			"the deploy time; it never runs or approves anything",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":` +
				`"string"},"pr_url":{"type":"string"},"deployed_at":{"type":"string",` +
				`"format":"date-time"},"commit":{"type":"string"}},` +
				`"required":["deployed_at"],"additionalProperties":false}`)}}
}

// changeReviewCaller is the bound principal as a Change Review caller.
func changeReviewCaller(ctx context.Context) changereview.Caller {
	return changereview.Caller{Actor: ActorFromContext(ctx), Source: changereview.SourceMCP}
}

func (s *Server) callChangeReviewTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(ChangeReviewBackend)
	if !ok {
		return nil, failure(codeUnavailable, "Change Review is unavailable on this server")
	}
	database, _ := DatabaseFromContext(ctx)
	call := reportChangeDeployed
	switch name {
	case "review_change":
		call = reviewChange
	case "get_change_review":
		call = getChangeReview
	case "list_change_reviews":
		call = listChangeReviews
	}
	result, failed := call(ctx, backend, database, raw)
	if failed != nil {
		return nil, failed
	}
	// A map result is the tools/call result itself; wrap it.
	return toolSuccess(result), nil
}

func reviewChange(ctx context.Context, b ChangeReviewBackend, database string,
	raw json.RawMessage) (any, *rpcError) {
	var req changereview.Request
	if !decodeStrict(raw, &req) {
		return nil, failure(codeInvalidParams, "invalid arguments for review_change")
	}
	req.Database = database
	review, created, err := b.ReviewChange(ctx, database, changeReviewCaller(ctx), req)
	if err != nil {
		return nil, changeReviewFailure(err)
	}
	return reviewView(review, &created)
}

func getChangeReview(ctx context.Context, b ChangeReviewBackend, database string,
	raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Database    string `json:"database"`
		ID          string `json:"id"`
		WaitSeconds int    `json:"wait_seconds"`
	}
	if !decodeStrict(raw, &args) || strings.TrimSpace(args.ID) == "" ||
		args.WaitSeconds < 0 || args.WaitSeconds > changereview.MaxWaitSeconds {
		return nil, failure(codeInvalidParams, fmt.Sprintf("invalid arguments: id is "+
			"required and wait_seconds is 0-%d", changereview.MaxWaitSeconds))
	}
	review, err := b.GetChangeReview(ctx, database, args.ID,
		time.Duration(args.WaitSeconds)*time.Second)
	if err != nil {
		return nil, changeReviewFailure(err)
	}
	return reviewView(review, nil)
}

func listChangeReviews(ctx context.Context, b ChangeReviewBackend, database string,
	raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Database string `json:"database"`
		changereview.ListFilter
	}
	if !decodeStrict(raw, &args) {
		return nil, failure(codeInvalidParams, "invalid arguments for list_change_reviews")
	}
	reviews, err := b.ListChangeReviews(ctx, database, args.ListFilter)
	if err != nil {
		return nil, changeReviewFailure(err)
	}
	views := make([]any, 0, len(reviews))
	for _, r := range reviews {
		view, failed := reviewView(r, nil)
		if failed != nil {
			return nil, failed
		}
		views = append(views, view)
	}
	return map[string]any{"reviews": views}, nil
}

func reportChangeDeployed(ctx context.Context, b ChangeReviewBackend, database string,
	raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Database string `json:"database"`
		changereview.Deployment
	}
	if !decodeStrict(raw, &args) {
		return nil, failure(codeInvalidParams, "invalid arguments: report_change_deployed "+
			"takes id or pr_url, deployed_at (RFC 3339) and commit")
	}
	review, err := b.ReportChangeDeployed(ctx, database, changeReviewCaller(ctx),
		args.Deployment)
	if err != nil {
		return nil, changeReviewFailure(err)
	}
	return reviewView(review, nil)
}

// reviewView is the review's JSON view (no transcript, with its comment
// marker) as an object, with created on review_change.
func reviewView(r changereview.Review, created *bool) (map[string]any, *rpcError) {
	raw, err := changereview.RenderJSON(r)
	var view map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &view)
	}
	if err != nil {
		return nil, failure(codeInternal, "internal error")
	}
	if created != nil {
		view["created"] = *created
	}
	return view, nil
}

// changeReviewFailure maps service errors to distinguishable tool errors;
// internal detail is not echoed.
func changeReviewFailure(err error) *rpcError {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(codeCancelled, "request cancelled")
	case errors.Is(err, changereview.ErrInvalid):
		return failure(codeInvalidParams, err.Error())
	case errors.Is(err, changereview.ErrNotFound):
		return failure(codeNotFound, "change review not found")
	case errors.Is(err, changereview.ErrDisabled):
		return failure(codeUnavailable, "Change Review is disabled (change_review.enabled: "+
			"false)")
	case errors.Is(err, changereview.ErrBusy):
		return failure(codeUnavailable, "Change Review is busy on this database: "+
			err.Error()+"; retry in a minute")
	case errors.Is(err, changereview.ErrAlreadyDecided):
		return failure(codeConflict, "already recorded: "+err.Error())
	case errors.Is(err, errChangeReviewNotRunning):
		return failure(codeUnavailable, err.Error())
	case errors.Is(err, changereview.ErrUnavailable),
		errors.Is(err, ErrProductionDependencyUnavailable):
		return failure(codeUnavailable, "Change Review is unavailable for this database; "+
			"retry later")
	}
	return failure(codeInternal, "internal error")
}
