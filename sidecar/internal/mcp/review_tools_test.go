package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// review_grade (review-01a): a read-only, per-database tool over the stored
// grade. The read itself is tested in internal/review/rubric and, through
// the route, in internal/api. No concurrent access tests: the tool keeps
// no state of its own.

type reviewRecordingBackend struct {
	recordingBackend
	calls    int
	database string
	err      error
}

func (b *reviewRecordingBackend) ReviewGrade(ctx context.Context,
	req ReviewGradeRequest) (any, error) {
	b.calls++
	b.database = req.Database
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"latest": nil}, nil
}

func TestReviewGradeToolIsReadOnlyPerDatabase(t *testing.T) {
	for _, tool := range NewServer(&reviewRecordingBackend{}).Tools() {
		if tool.Name != toolReviewGrade {
			continue
		}
		require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
		require.True(t, strings.Contains(string(tool.InputSchema), `"database"`),
			"takes the database argument")
		require.True(t, strings.Contains(tool.Description, "Read-only"), tool.Description)
		return
	}
	t.Fatal("review_grade is not listed")
}

func TestReviewGradeToolBindsTheDatabaseAndViewerMayCall(t *testing.T) {
	backend := &reviewRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3",
		Role: "viewer"})
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, viewer, toolCall(toolReviewGrade, `{}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "orders", backend.database)
}

func TestReviewGradeToolRejectsExtraArguments(t *testing.T) {
	backend := &reviewRecordingBackend{}
	for _, args := range []string{`{"x":1}`, `{"kind":"review"}`, `[]`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(toolReviewGrade, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
}

func TestReviewGradeToolErrors(t *testing.T) {
	failing := &reviewRecordingBackend{err: errors.New("read sage.review_run: refused")}
	response := invoke(t, NewServer(failing), context.Background(),
		toolCall(toolReviewGrade, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	require.True(t, strings.Contains(response.Error.Message, "sage.review_run"),
		response.Error.Message)
	cancelled := &reviewRecordingBackend{err: context.Canceled}
	response = invoke(t, NewServer(cancelled), context.Background(),
		toolCall(toolReviewGrade, `{}`))
	require.Equal(t, codeCancelled, response.Error.Code, "cancelled")
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall(toolReviewGrade, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).ReviewGrade(context.Background(), ReviewGradeRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "unwired")
}
