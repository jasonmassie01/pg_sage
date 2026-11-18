package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// review_scorecard (review-05a) is the resolved database's Sage Review
// scorecard: steps proposed, accepted, verified improved, no change,
// inconclusive, regressed, reverted and unverifiable by origin and by
// generator, with the latest model gate (G-M1, G-M2) evaluations. The same
// read as GET /api/v1/databases/{db}/review/scorecard; it never evaluates
// or approves the gate.

const toolReviewScorecard = "review_scorecard"

// ReviewScorecardRequest is the database the server resolved and an
// optional start of the window (default: the last 90 days).
type ReviewScorecardRequest struct {
	Database string     `json:"database,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
}

// ReviewScorecardBackend serves review_scorecard.
type ReviewScorecardBackend interface {
	ReviewScorecard(context.Context, ReviewScorecardRequest) (any, error)
}

func reviewScorecardTool() Tool {
	return Tool{Name: toolReviewScorecard, Description: "Sage Review's scorecard for the " +
		"database: per review and overall, the plan steps each origin (generator, model, " +
		"person) proposed and how many were accepted, verified improved, no change, " +
		"inconclusive, regressed, reverted or unverifiable, by generator too, and the " +
		"latest model gate evaluations (G-M1: may the model's steps run without their own " +
		"approval; G-M2: does the model pass still add steps of its own). Read-only",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"since":{` +
			`"type":"string","format":"date-time","description":"Start of the window ` +
			`(RFC 3339); default the last 90 days"}},"additionalProperties":false}`)}
}

func (s *Server) callReviewScorecardTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var req ReviewScorecardRequest
	if !decodeStrict(raw, &req) {
		return nil, failure(codeInvalidParams, "invalid arguments: review_scorecard takes "+
			"only since (an RFC 3339 time)")
	}
	if db, ok := DatabaseFromContext(ctx); ok {
		req.Database = db
	}
	backend, ok := s.backend.(ReviewScorecardBackend)
	if !ok {
		return nil, failure(codeInternal, "review scorecard unavailable")
	}
	result, err := backend.ReviewScorecard(ctx, req)
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, failure(codeCancelled, "request cancelled")
	case err != nil:
		return nil, failure(codeInternal, "review scorecard not read: "+err.Error())
	}
	return result, nil
}

// ReviewScorecard serves review_scorecard.
func (backend *ProductionBackend) ReviewScorecard(ctx context.Context,
	request ReviewScorecardRequest) (any, error) {
	if backend.dependencies.ReviewScorecard == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.ReviewScorecard.ReviewScorecard(ctx, request)
}
