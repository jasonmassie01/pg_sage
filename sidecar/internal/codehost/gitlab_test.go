package codehost

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Recorded answers are trimmed from the GitLab REST API v4 documentation's
// examples; recording against a real project is OWNER ACTION.

// glProjectPath is acme/backend/shop with its slashes escaped, as GitLab
// wants a namespaced project id.
const glProjectPath = "/projects/acme%2Fbackend%2Fshop"

func newTestGitLab(t *testing.T, rec *recorder) *GitLab {
	t.Helper()
	gl, err := NewGitLab(Options{BaseURL: rec.srv.URL + "/api/v4", Token: testToken})
	require.NoError(t, err)
	return gl
}

func TestGitLabSearchCodeKeepsEachFileOnce(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath + "/search": {fixture: "gitlab/search_blobs.json"}})
	hits, err := newTestGitLab(t, rec).SearchCode(context.Background(), "acme/backend/shop",
		"orders index")
	require.NoError(t, err)
	require.Equal(t, []SearchHit{{Path: "app/controllers/orders_controller.rb"},
		{Path: "lib/orders.rb"}}, hits)
	req := rec.requests()[0]
	q, _ := url.ParseQuery(req.Query)
	require.Equal(t, "blobs", q.Get("scope"))
	require.Equal(t, "orders index", q.Get("search"))
	require.Equal(t, testToken, req.Header.Get("PRIVATE-TOKEN"))
	require.Equal(t, "", req.Header.Get("Authorization"))
}

func TestGitLabGetFileReadsRawOnDefaultBranch(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath + "/repository/files/app%2Fmodels%2Forder.rb/raw": {
			body: "class Order\nend\n"}})
	f, err := newTestGitLab(t, rec).GetFile(context.Background(), "acme/backend/shop",
		"app/models/order.rb")
	require.NoError(t, err)
	require.Equal(t, "class Order\nend\n", f.Content)
	q, _ := url.ParseQuery(rec.requests()[0].Query)
	require.Equal(t, "HEAD", q.Get("ref"))
}

func TestGitLabListOpenMergeRequests(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath + "/merge_requests": {
			fixture: "gitlab/merge_requests_open.json"}})
	prs, err := newTestGitLab(t, rec).ListOpenPRs(context.Background(), "acme/backend/shop",
		"pg_sage")
	require.NoError(t, err)
	require.Equal(t, []PullRequest{{Number: 12, Title: "pg_sage: add index",
		Body:   "<!-- pg_sage:key=cand-1 -->",
		URL:    "https://gitlab.example.com/acme/backend/shop/-/merge_requests/12",
		Labels: []string{"pg_sage"}}}, prs)
	q, _ := url.ParseQuery(rec.requests()[0].Query)
	require.Equal(t, "opened", q.Get("state"))
	require.Equal(t, "pg_sage", q.Get("labels"))
}

func TestGitLabOpenPROneCommitThenMergeRequest(t *testing.T) {
	files := "/api/v4" + glProjectPath + "/repository/files/"
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath:                          {fixture: "gitlab/project.json"},
		"HEAD " + files + "db%2Fschema.sql":                    {status: 200},
		"POST /api/v4" + glProjectPath + "/repository/commits": {status: 201, body: `{}`},
		"POST /api/v4" + glProjectPath + "/merge_requests": {status: 201,
			fixture: "gitlab/merge_request_created.json"},
	})
	pr, err := newTestGitLab(t, rec).OpenPR(context.Background(), "acme/backend/shop",
		PRRequest{Title: "pg_sage: add index", Body: "body", Branch: "pg_sage/cand-1",
			Labels: []string{"pg_sage", "db"}, Files: []FileChange{
				{Path: "db/migrate/1_idx.sql", Content: "CREATE INDEX"},
				{Path: "db/schema.sql", Content: "-- schema"}}})
	require.NoError(t, err)
	require.Equal(t, 13, pr.Number)
	reqs := rec.requests()
	require.Len(t, reqs, 5)
	var actions []map[string]string
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(reqs[3].Body), &raw))
	require.JSONEq(t, `"pg_sage/cand-1"`, string(raw["branch"]))
	require.JSONEq(t, `"main"`, string(raw["start_branch"]))
	require.NoError(t, json.Unmarshal(raw["actions"], &actions))
	require.Equal(t, "create", actions[0]["action"], "absent on the base")
	require.Equal(t, "update", actions[1]["action"], "present on the base")
	var mr map[string]string
	require.NoError(t, json.Unmarshal([]byte(reqs[4].Body), &mr))
	require.Equal(t, "pg_sage,db", mr["labels"])
	require.Equal(t, "main", mr["target_branch"])
}

func TestGitLabPRStates(t *testing.T) {
	mr := "/api/v4" + glProjectPath + "/merge_requests/"
	rec := newRecorder(t, map[string]answer{
		"GET " + mr + "12":        {fixture: "gitlab/merge_request_merged.json"},
		"GET " + mr + "13":        {body: `{"iid":13,"state":"locked"}`},
		"GET " + mr + "14":        {body: `{"iid":14,"state":"opened"}`},
		"GET " + mr + "15":        {body: `{"iid":15,"state":"draft?"}`},
		"POST " + mr + "12/notes": {status: 201, body: `{"id":1}`},
	})
	gl := newTestGitLab(t, rec)
	ctx := context.Background()
	st, err := gl.PRState(ctx, "acme/backend/shop", 12)
	require.NoError(t, err)
	require.Equal(t, PRMerged, st.State)
	require.Equal(t, time.Date(2026, 11, 10, 14, 21, 7, 0, time.UTC), st.MergedAt.UTC())
	st, err = gl.PRState(ctx, "acme/backend/shop", 13)
	require.NoError(t, err)
	require.Equal(t, PRClosed, st.State)
	st, err = gl.PRState(ctx, "acme/backend/shop", 14)
	require.NoError(t, err)
	require.Equal(t, PROpen, st.State)
	_, err = gl.PRState(ctx, "acme/backend/shop", 15)
	require.ErrorIs(t, err, ErrUnavailable)
	require.NoError(t, gl.Comment(ctx, "acme/backend/shop", 12, "verdict: improved"))
	_, err = gl.PRState(ctx, "acme/backend/shop", 99)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGitLab429IsARateLimit(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath + "/merge_requests/1": {status: 429,
			header: map[string]string{"Retry-After": "7"}}})
	_, err := newTestGitLab(t, rec).PRState(context.Background(), "acme/backend/shop", 1)
	require.ErrorIs(t, err, ErrRateLimited)
	require.ErrorContains(t, err, "retry after 7s")
}
