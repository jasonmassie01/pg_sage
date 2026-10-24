package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The registry adapter on real Postgres: MCP calls reach the database's
// Change Review service, store the review in that database's
// sage.change_review and land it with source reported (CHECK-SRF-02,
// CHECK-SRF-06). The analyzer is a fake; nothing reaches an LLM.

func newCRRegistry(t *testing.T, mutate func(*changereview.Deps)) (*changereview.Registry,
	*crfake.Analyzer) {
	t.Helper()
	pool := crfake.Pool(t)
	analyzer := &crfake.Analyzer{}
	reg := changereview.NewRegistry()
	reg.Set("orders", crfake.Start(t, pool, "orders", analyzer, mutate))
	return reg, analyzer
}

type crServerBackend struct {
	recordingBackend
	ChangeReviewBackend
}

func crServer(reg *changereview.Registry) *Server {
	return NewServer(&crServerBackend{ChangeReviewBackend: NewChangeReviewBackend(reg)})
}

func TestChangeReviewBackendRunsAReviewEndToEnd(t *testing.T) {
	reg, analyzer := newCRRegistry(t, nil)
	server := crServer(reg)
	ctx := agentContext(ScopeRead, ScopePropose)
	args := `{"database":"orders","sql":"ALTER TABLE t DROP COLUMN c; -- risky",` +
		`"pr":{"url":"https://github.com/o/r/pull/9","head_sha":"abcdef1"},"wait_seconds":20}`
	first := structuredContent(t, invoke(t, server, ctx, toolCall("review_change", args)))
	require.Equal(t, "complete", first["state"])
	require.Equal(t, "risky", first["verdict"])
	require.Equal(t, true, first["created"])
	require.Equal(t, "mcp", first["source"])
	require.Equal(t, "mcp:token:agt1", first["actor"])
	require.True(t, strings.HasPrefix(first["markdown"].(string), first["marker"].(string)))
	id := first["id"].(string)

	again := structuredContent(t, invoke(t, server, ctx, toolCall("review_change", args)))
	require.Equal(t, id, again["id"])
	require.Equal(t, false, again["created"])
	require.Equal(t, 1, analyzer.Calls())

	got := structuredContent(t, invoke(t, server, ctx, toolCall("get_change_review",
		`{"database":"orders","id":"`+id+`"}`)))
	require.Equal(t, id, got["id"])
	listed := structuredContent(t, invoke(t, server, ctx, toolCall("list_change_reviews",
		`{"database":"orders","pr_url":"https://github.com/o/r/pull/9"}`)))
	reviews, _ := listed["reviews"].([]any)
	require.Equal(t, 1, len(reviews))

	deployed := structuredContent(t, invoke(t, server, ctx, toolCall("report_change_deployed",
		`{"database":"orders","pr_url":"https://github.com/o/r/pull/9","deployed_at":"`+
			time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)+`"}`)))
	require.Equal(t, id, deployed["id"])
	require.Equal(t, "landed", deployed["landing_state"])
	require.Equal(t, "reported", deployed["landing_source"])

	twice := invoke(t, server, ctx, toolCall("report_change_deployed",
		`{"database":"orders","id":"`+id+`","deployed_at":"`+
			time.Now().UTC().Format(time.RFC3339)+`"}`))
	require.Equal(t, -32009, twice.Error.Code, "a landed review lands once")
}

func TestChangeReviewBackendResolvesTheDatabase(t *testing.T) {
	reg, _ := newCRRegistry(t, nil)
	backend := NewChangeReviewBackend(reg)
	caller := changereview.Caller{Actor: "mcp:token:x", Source: changereview.SourceMCP}
	r, created, err := backend.ReviewChange(context.Background(), "", caller,
		changereview.Request{SQL: "SELECT 1", WaitSeconds: 20})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "orders", r.Database, "the only registered database is the default")

	_, _, err = backend.ReviewChange(context.Background(), "billing", caller,
		changereview.Request{SQL: "SELECT 1"})
	require.True(t, errors.Is(err, changereview.ErrUnavailable), "%v", err)
	require.Contains(t, err.Error(), "billing")
	_, err = backend.GetChangeReview(context.Background(), "billing", r.ID, 0)
	require.True(t, errors.Is(err, changereview.ErrUnavailable), "%v", err)

	reg.Set("billing", nil) // ignored: a nil service is never registered
	require.Equal(t, []string{"orders"}, reg.Databases())
	_, err = backend.GetChangeReview(context.Background(), "orders",
		"00000000-0000-0000-0000-000000000099", 0)
	require.True(t, errors.Is(err, changereview.ErrNotFound), "%v", err)
}

func TestChangeReviewBackendNeedsANameWithSeveralDatabases(t *testing.T) {
	reg, _ := newCRRegistry(t, nil)
	reg.Set("billing", crfake.Start(t, crfake.Pool(t), "billing", &crfake.Analyzer{}, nil))
	_, err := NewChangeReviewBackend(reg).ListChangeReviews(context.Background(), "",
		changereview.ListFilter{})
	require.True(t, errors.Is(err, changereview.ErrInvalid), "%v", err)
	require.Contains(t, err.Error(), "billing, orders")
	var nilBackend ChangeReviewBackend = NewChangeReviewBackend(nil)
	_, err = nilBackend.ListChangeReviews(context.Background(), "orders",
		changereview.ListFilter{})
	require.True(t, errors.Is(err, changereview.ErrUnavailable), "%v", err)
}

func TestChangeReviewBackendMapsDisabledAndBusyOverMCP(t *testing.T) {
	reg, _ := newCRRegistry(t, func(d *changereview.Deps) { d.Config.Enabled = false })
	response := invoke(t, crServer(reg), agentContext(ScopeRead), toolCall("review_change",
		`{"database":"orders","sql":"SELECT 1"}`))
	require.Equal(t, -32010, response.Error.Code)
	require.Contains(t, response.Error.Message, "disabled")

	busyReg, analyzer := newCRRegistry(t, func(d *changereview.Deps) {
		d.Config.MaxConcurrentPerDatabase = 1
	})
	analyzer.Gate = make(chan struct{})
	defer close(analyzer.Gate)
	server := crServer(busyReg)
	for i := 0; i < 1+changereview.QueueLength; i++ {
		args := `{"database":"orders","sql":"SELECT ` + string(rune('a'+i)) + `"}`
		r := structuredContent(t, invoke(t, server, agentContext(ScopeRead),
			toolCall("review_change", args)))
		require.Equal(t, "running", r["state"])
	}
	response = invoke(t, server, agentContext(ScopeRead), toolCall("review_change",
		`{"database":"orders","sql":"SELECT 42"}`))
	require.Equal(t, -32010, response.Error.Code)
	require.Contains(t, strings.ToLower(response.Error.Message), "retry")
}
