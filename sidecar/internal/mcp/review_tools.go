package mcp

import (
	"context"
	"encoding/json"
	"errors"
)

// review_grade (review-01a) is the resolved database's latest Sage Review
// grade against the rubric, the previous grade and the delta between
// them: the same read as GET /api/v1/databases/{db}/review/grade. It only
// reads the stored runs; it never starts a grade.

const toolReviewGrade = "review_grade"

// ReviewGradeRequest is the database the server resolved.
type ReviewGradeRequest struct {
	Database string `json:"database,omitempty"`
}

// ReviewGradeBackend serves review_grade.
type ReviewGradeBackend interface {
	ReviewGrade(context.Context, ReviewGradeRequest) (any, error)
}

func reviewTools() []Tool {
	return []Tool{{Name: toolReviewGrade,
		InputSchema: json.RawMessage(`{"type":"object","properties":{},` +
			`"additionalProperties":false}`)}, reviewScorecardTool()}
}

func (s *Server) callReviewTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var req ReviewGradeRequest
	if !decodeStrict(raw, &req) {
		return nil, failure(codeInvalidParams, "invalid arguments: review_grade takes "+
			"only database")
	}
	if db, ok := DatabaseFromContext(ctx); ok {
		req.Database = db
	}
	backend, ok := s.backend.(ReviewGradeBackend)
	if !ok {
		return nil, failure(codeInternal, "review grade unavailable")
	}
	result, err := backend.ReviewGrade(ctx, req)
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, failure(codeCancelled, "request cancelled")
	case err != nil:
		return nil, failure(codeInternal, "review grade not read: "+err.Error())
	}
	return result, nil
}

// ReviewGrade serves review_grade.
func (backend *ProductionBackend) ReviewGrade(ctx context.Context,
	request ReviewGradeRequest) (any, error) {
	if backend.dependencies.ReviewGrade == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.ReviewGrade.ReviewGrade(ctx, request)
}
