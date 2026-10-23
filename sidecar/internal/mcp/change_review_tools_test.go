package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Change Review over MCP (CHECK-SRF-02, -06). review_change,
// get_change_review and list_change_reviews need the read scope;
// report_change_deployed the propose scope. No MCP tool records a DBA
// verdict: that is a person's decision in the UI or REST.

type crRecordingBackend struct {
	recordingBackend
	calls    []string
	database string
	caller   changereview.Caller
	request  changereview.Request
	id       string
	wait     time.Duration
	filter   changereview.ListFilter
	deploy   changereview.Deployment
	err      error
}

func (b *crRecordingBackend) review() changereview.Review {
	return changereview.Review{ID: "00000000-0000-0000-0000-0000000000aa", Database: b.database,
		State: changereview.StateComplete, Verdict: changereview.VerdictSafe,
		Markdown: "<!-- pg_sage-review -->", ShadowScore: changereview.ShadowUnscored,
		CreditState: changereview.CreditNone}
}

func (b *crRecordingBackend) ReviewChange(_ context.Context, database string,
	c changereview.Caller, r changereview.Request) (changereview.Review, bool, error) {
	b.calls = append(b.calls, "review_change")
	b.database, b.caller, b.request = database, c, r
	return b.review(), true, b.err
}

func (b *crRecordingBackend) GetChangeReview(_ context.Context, database, id string,
	wait time.Duration) (changereview.Review, error) {
	b.calls = append(b.calls, "get_change_review")
	b.database, b.id, b.wait = database, id, wait
	return b.review(), b.err
}

func (b *crRecordingBackend) ListChangeReviews(_ context.Context, database string,
	f changereview.ListFilter) ([]changereview.Review, error) {
	b.calls = append(b.calls, "list_change_reviews")
	b.database, b.filter = database, f
	return []changereview.Review{b.review(), b.review()}, b.err
}

func (b *crRecordingBackend) ReportChangeDeployed(_ context.Context, database string,
	c changereview.Caller, d changereview.Deployment) (changereview.Review, error) {
	b.calls = append(b.calls, "report_change_deployed")
	b.database, b.caller, b.deploy = database, c, d
	return b.review(), b.err
}

var crTools = map[string]Scope{"review_change": ScopeRead, "get_change_review": ScopeRead,
	"list_change_reviews": ScopeRead, "report_change_deployed": ScopePropose}

const (
	crReviewArgs = `{"database":"orders","sql":"ALTER TABLE t ADD COLUMN c int",` +
		`"sql_files":[{"path":"db/1.sql","content":"SELECT 1"}],"diff":"--- a\n+++ b\n",` +
		`"pr":{"url":"https://github.com/o/r/pull/7","number":7,"title":"Add c",` +
		`"repo":"o/r","head_sha":"abcdef1","base_sha":"1234567",` +
		`"opened_at":"2026-10-20T10:00:00Z"},"migration_tool":"flyway",` +
		`"transactional":false,"wait_seconds":30}`
	crGetArgs = `{"database":"orders","id":"00000000-0000-0000-0000-0000000000aa",` +
		`"wait_seconds":5}`
	crListArgs = `{"database":"orders","pr_url":"https://github.com/o/r/pull/7",` +
		`"state":"complete","limit":3}`
	crDeployArgs = `{"database":"orders","pr_url":"https://github.com/o/r/pull/7",` +
		`"deployed_at":"2026-10-21T09:30:00Z","commit":"abcdef1"}`
)

func TestChangeReviewToolScopes(t *testing.T) {
	for name, want := range crTools {
		scope, ok := RequiredScope(name, nil)
		require.True(t, ok, name)
		require.Equal(t, want, scope, name)
	}
	require.True(t, proposeScopeTools["report_change_deployed"])
	for _, verdictTool := range []string{"decide_change_review", "record_dba_verdict"} {
		_, ok := RequiredScope(verdictTool, nil)
		require.False(t, ok, "no MCP tool records a DBA verdict: %s", verdictTool)
	}
}

func TestChangeReviewToolsAreListedWithTheirSchemas(t *testing.T) {
	want := map[string][]string{
		"review_change": {"database", "sql", "sql_files", "diff", "pr", "migration_tool",
			"transactional", "wait_seconds"},
		"get_change_review":      {"database", "id", "wait_seconds"},
		"list_change_reviews":    {"database", "pr_url", "state", "limit"},
		"report_change_deployed": {"database", "id", "pr_url", "deployed_at", "commit"},
	}
	found := map[string]bool{}
	for _, tool := range NewServer(&crRecordingBackend{}).Tools() {
		props, ok := want[tool.Name]
		if !ok {
			continue
		}
		found[tool.Name] = true
		schema := decodeSchema(t, tool.InputSchema)
		properties := objectMap(t, schema["properties"])
		require.Equal(t, len(props), len(properties), tool.Name)
		for _, p := range props {
			_, ok := properties[p]
			require.True(t, ok, "%s.%s missing", tool.Name, p)
		}
		require.Equal(t, false, schema["additionalProperties"], tool.Name)
		require.True(t, tool.Annotations != nil, tool.Name)
		require.Equal(t, tool.Name != "report_change_deployed", tool.Annotations.ReadOnlyHint,
			tool.Name)
	}
	require.Equal(t, 4, len(found))
}

func TestChangeReviewToolsPassTheRequestThrough(t *testing.T) {
	ctx := agentContext(ScopeRead, ScopePropose)
	backend := &crRecordingBackend{}
	server := NewServer(backend)
	response := invoke(t, server, ctx, toolCall("review_change", crReviewArgs))
	require.Empty(t, response.Error.Code)
	content := structuredContent(t, response)
	require.Equal(t, true, content["created"])
	require.Equal(t, "safe", content["verdict"])
	require.True(t, strings.HasPrefix(fmt.Sprint(content["marker"]), "<!-- pg_sage-review db="))
	_, hasTranscript := content["transcript"]
	require.False(t, hasTranscript)
	require.Equal(t, changereview.Caller{Actor: "mcp:token:agt1",
		Source: changereview.SourceMCP}, backend.caller)
	r := backend.request
	require.Equal(t, "orders", backend.database)
	require.Equal(t, "orders", r.Database)
	require.Equal(t, "ALTER TABLE t ADD COLUMN c int", r.SQL)
	require.Equal(t, []changereview.SQLFile{{Path: "db/1.sql", Content: "SELECT 1"}},
		r.SQLFiles)
	require.Equal(t, "--- a\n+++ b\n", r.Diff)
	require.Equal(t, "https://github.com/o/r/pull/7", r.PR.URL)
	require.Equal(t, 7, r.PR.Number)
	require.Equal(t, "abcdef1", r.PR.HeadSHA)
	require.True(t, r.PR.OpenedAt != nil && r.PR.OpenedAt.Equal(
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)))
	require.Equal(t, "flyway", r.MigrationTool)
	require.True(t, r.Transactional != nil && !*r.Transactional)
	require.Equal(t, 30, r.WaitSeconds)

	response = invoke(t, server, ctx, toolCall("get_change_review", crGetArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "00000000-0000-0000-0000-0000000000aa", backend.id)
	require.Equal(t, 5*time.Second, backend.wait)

	response = invoke(t, server, ctx, toolCall("list_change_reviews", crListArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, changereview.ListFilter{PRURL: "https://github.com/o/r/pull/7",
		State: "complete", Limit: 3}, backend.filter)
	reviews, _ := structuredContent(t, response)["reviews"].([]any)
	require.Equal(t, 2, len(reviews))

	response = invoke(t, server, ctx, toolCall("report_change_deployed", crDeployArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "https://github.com/o/r/pull/7", backend.deploy.PRURL)
	require.Equal(t, "abcdef1", backend.deploy.Commit)
	require.True(t, backend.deploy.DeployedAt.Equal(time.Date(2026, 10, 21, 9, 30, 0, 0,
		time.UTC)))
	require.Equal(t, []string{"review_change", "get_change_review", "list_change_reviews",
		"report_change_deployed"}, backend.calls)
}

func TestChangeReviewDeployNeedsProposeScope(t *testing.T) {
	backend := &crRecordingBackend{}
	response := invoke(t, NewServer(backend), agentContext(ScopeRead),
		toolCall("report_change_deployed", crDeployArgs))
	require.Equal(t, -32001, response.Error.Code)
	require.Empty(t, backend.calls)
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	response = invoke(t, NewServer(backend), viewer, toolCall("report_change_deployed",
		crDeployArgs))
	require.Equal(t, -32001, response.Error.Code)
	for name, args := range map[string]string{"review_change": crReviewArgs,
		"get_change_review": crGetArgs, "list_change_reviews": crListArgs} {
		response = invoke(t, NewServer(backend), agentContext(ScopeRead), toolCall(name, args))
		require.Empty(t, response.Error.Code, name)
	}
	require.Equal(t, 3, len(backend.calls))
}

type twoDatabases struct{}

func (twoDatabases) Databases() []DatabaseRef {
	return []DatabaseRef{{Name: "orders"}, {Name: "billing"}}
}

func TestChangeReviewHonorsTheTokenDatabaseBinding(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "token:ci", Kind: KindAgent,
		Scopes: []Scope{ScopeRead, ScopePropose}, Databases: []string{"orders"}})
	backend := &crRecordingBackend{}
	server := NewServer(backend).WithDirectory(twoDatabases{})
	other := strings.Replace(crReviewArgs, `"database":"orders"`, `"database":"billing"`, 1)
	response := invoke(t, server, ctx, toolCall("review_change", other))
	require.Equal(t, -32003, response.Error.Code)
	require.Empty(t, backend.calls)
	unnamed := strings.Replace(crReviewArgs, `"database":"orders",`, ``, 1)
	response = invoke(t, server, ctx, toolCall("review_change", unnamed))
	require.Equal(t, -32006, response.Error.Code, "two databases: one must be named")
	response = invoke(t, server, ctx, toolCall("review_change", crReviewArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "orders", backend.database)
}

func TestChangeReviewToolsRejectMalformedArguments(t *testing.T) {
	ctx := agentContext(ScopeRead, ScopePropose)
	cases := map[string]string{
		"review_change unknown field":  `{"database":"orders","sql":"x","verdict":"safe"}`,
		"review_change wait type":      `{"database":"orders","sql":"x","wait_seconds":"9"}`,
		"review_change sql_files type": `{"database":"orders","sql_files":"a.sql"}`,
		"get_change_review no id":      `{"database":"orders"}`,
		"get_change_review wait 51":    `{"database":"orders","id":"x","wait_seconds":51}`,
		"get_change_review wait -1":    `{"database":"orders","id":"x","wait_seconds":-1}`,
		"list limit type":              `{"database":"orders","limit":"5"}`,
		"deployed bad time":            `{"database":"orders","id":"x","deployed_at":"yesterday"}`,
		"deployed unknown field":       `{"database":"orders","id":"x","approve":true}`,
	}
	for name, args := range cases {
		tool := strings.SplitN(name, " ", 2)[0]
		if tool == "list" {
			tool = "list_change_reviews"
		} else if tool == "deployed" {
			tool = "report_change_deployed"
		}
		backend := &crRecordingBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall(tool, args))
		require.Equal(t, -32602, response.Error.Code, name)
		require.Empty(t, backend.calls, name)
	}
}

func TestChangeReviewToolsMapServiceErrors(t *testing.T) {
	ctx := agentContext(ScopeRead, ScopePropose)
	cases := []struct {
		err  error
		code int
		text string
	}{
		{fmt.Errorf("%w: the SQL is 9 bytes, over max_sql_bytes", changereview.ErrInvalid),
			-32602, "max_sql_bytes"},
		{fmt.Errorf("%w: review x", changereview.ErrNotFound), -32004, "not found"},
		{changereview.ErrDisabled, -32010, "change_review.enabled"},
		{fmt.Errorf("%w: 10 reviews running", changereview.ErrBusy), -32010, "retry"},
		{fmt.Errorf("%w: landed", changereview.ErrAlreadyDecided), -32009, "already"},
		{fmt.Errorf("%w: store", changereview.ErrUnavailable), -32010, "unavailable"},
		{errors.New("dial tcp 10.0.0.9:5432"), -32603, "internal"},
	}
	for _, tc := range cases {
		backend := &crRecordingBackend{err: tc.err}
		response := invoke(t, NewServer(backend), ctx, toolCall("review_change",
			crReviewArgs))
		require.Equal(t, tc.code, response.Error.Code, tc.err.Error())
		require.Contains(t, strings.ToLower(response.Error.Message), tc.text, tc.err.Error())
		require.NotContains(t, response.Error.Message, "10.0.0.9")
	}
	plain := invoke(t, NewServer(&recordingBackend{}), ctx, toolCall("review_change",
		crReviewArgs))
	require.Equal(t, -32010, plain.Error.Code, "a server without Change Review")
}

func TestProductionBackendWithoutChangeReviewIsUnavailable(t *testing.T) {
	backend := &ProductionBackend{}
	_, _, err := backend.ReviewChange(context.Background(), "orders",
		changereview.Caller{}, changereview.Request{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable))
	_, err = backend.GetChangeReview(context.Background(), "orders", "x", 0)
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable))
	_, err = backend.ListChangeReviews(context.Background(), "orders",
		changereview.ListFilter{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable))
	_, err = backend.ReportChangeDeployed(context.Background(), "orders",
		changereview.Caller{}, changereview.Deployment{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable))
}
