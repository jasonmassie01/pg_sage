package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// review_scorecard (review-05a): read-only and per database. The read is
// tested in internal/review/scorecard and, through the route, internal/api.
// No concurrent access tests: the tool keeps no state of its own.

type scorecardRecordingBackend struct {
	recordingBackend
	calls int
	req   ReviewScorecardRequest
	err   error
}

func (b *scorecardRecordingBackend) ReviewScorecard(_ context.Context,
	req ReviewScorecardRequest) (any, error) {
	b.calls++
	b.req = req
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"scorecard": map[string]any{"reviews": 0}}, nil
}

func TestReviewScorecardToolIsReadOnlyAndBindsTheDatabase(t *testing.T) {
	found := false
	for _, tool := range NewServer(&scorecardRecordingBackend{}).Tools() {
		if tool.Name == toolReviewScorecard {
			found = true
			require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint,
				"read-only")
			require.True(t, strings.Contains(tool.Description, "Read-only"), tool.Description)
		}
	}
	require.True(t, found, "review_scorecard is listed")
	backend := &scorecardRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3",
		Role: "viewer"})
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, viewer, toolCall(toolReviewScorecard,
		`{"since":"2026-11-01T00:00:00Z"}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "orders", backend.req.Database)
	require.True(t, backend.req.Since != nil &&
		backend.req.Since.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)), "since kept")
}

func TestReviewScorecardToolRejectsBadArgumentsAndSurfacesErrors(t *testing.T) {
	backend := &scorecardRecordingBackend{}
	for _, args := range []string{`{"x":1}`, `{"since":"yesterday"}`, `[]`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(toolReviewScorecard, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
	failing := &scorecardRecordingBackend{err: errors.New(`scorecard query "review runs"`)}
	response := invoke(t, NewServer(failing), context.Background(),
		toolCall(toolReviewScorecard, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	require.True(t, strings.Contains(response.Error.Message, "review runs"),
		response.Error.Message)
	response = invoke(t, NewServer(&scorecardRecordingBackend{err: context.Canceled}),
		context.Background(), toolCall(toolReviewScorecard, `{}`))
	require.Equal(t, codeCancelled, response.Error.Code, "cancelled")
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall(toolReviewScorecard, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).ReviewScorecard(context.Background(),
		ReviewScorecardRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "unwired")
}
