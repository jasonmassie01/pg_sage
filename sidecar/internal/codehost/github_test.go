package codehost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Recorded answers are trimmed from the GitHub REST API documentation's
// examples; recording against a real repository is OWNER ACTION.

const ghRepoPath = "/repos/acme/shop"

func newTestGitHub(t *testing.T, rec *recorder) *GitHub {
	t.Helper()
	gh, err := NewGitHub(Options{BaseURL: rec.srv.URL, Token: testToken})
	require.NoError(t, err)
	return gh
}

func TestGitHubSearchCodeScopesToRepoAndDropsUnsafePaths(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET /search/code": {fixture: "github/search_code.json"}})
	hits, err := newTestGitHub(t, rec).SearchCode(context.Background(), "acme/shop",
		"Orders index")
	require.NoError(t, err)
	require.Equal(t, []SearchHit{{Path: "app/controllers/orders_controller.rb"},
		{Path: "app/controllers/admin/orders_controller.rb"}}, hits)
	req := rec.requests()[0]
	q, _ := url.ParseQuery(req.Query)
	require.Equal(t, "Orders index repo:acme/shop", q.Get("q"))
	require.Equal(t, "20", q.Get("per_page"))
	require.Equal(t, "Bearer "+testToken, req.Header.Get("Authorization"))
	require.Equal(t, "2022-11-28", req.Header.Get("X-GitHub-Api-Version"))
	require.Equal(t, "application/vnd.github+json", req.Header.Get("Accept"))
}

func TestGitHubGetFileDecodesWrappedBase64(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/contents/app/models/order.rb": {
			fixture: "github/contents_file.json"},
		"GET " + ghRepoPath + "/contents/app/models": {fixture: "github/contents_dir.json"},
		"GET " + ghRepoPath + "/contents/app/models/a%20b.rb": {body: `{"type":"file",` +
			`"encoding":"none","size":5242880,"content":""}`},
	})
	gh := newTestGitHub(t, rec)
	f, err := gh.GetFile(context.Background(), "acme/shop", "app/models/order.rb")
	require.NoError(t, err)
	require.Equal(t, "class Order < ApplicationRecord\nscope :open\n", f.Content)
	require.Equal(t, "app/models/order.rb", f.Path)
	_, err = gh.GetFile(context.Background(), "acme/shop", "app/models")
	require.Error(t, err, "a directory listing is not a file")
	_, err = gh.GetFile(context.Background(), "acme/shop", "app/models/a b.rb")
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorContains(t, err, "larger than 1048576 bytes")
	_, err = gh.GetFile(context.Background(), "acme/shop", "app/missing.rb")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGitHubListOpenPRsKeepsOnlyPullRequests(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/issues": {fixture: "github/issues_open.json"}})
	prs, err := newTestGitHub(t, rec).ListOpenPRs(context.Background(), "acme/shop",
		"pg_sage")
	require.NoError(t, err)
	require.Len(t, prs, 1)
	require.Equal(t, 1347, prs[0].Number)
	require.Contains(t, prs[0].Body, "<!-- pg_sage:key=cand-1 -->")
	require.Equal(t, []string{"pg_sage"}, prs[0].Labels)
	require.Equal(t, "https://github.com/acme/shop/pull/1347", prs[0].URL)
	q, _ := url.ParseQuery(rec.requests()[0].Query)
	require.Equal(t, "pg_sage", q.Get("labels"))
	require.Equal(t, "open", q.Get("state"))
}

func TestGitHubOpenPRSequence(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath:                         {fixture: "github/repo.json"},
		"GET " + ghRepoPath + "/git/ref/heads/main": {fixture: "github/ref_main.json"},
		"POST " + ghRepoPath + "/git/refs":          {status: 201, body: `{}`},
		"PUT " + ghRepoPath + "/contents/db/migrate/20261115_idx.sql": {
			status: 201, body: `{}`},
		"POST " + ghRepoPath + "/pulls":              {fixture: "github/pull_created.json"},
		"POST " + ghRepoPath + "/issues/1350/labels": {body: `[]`},
	})
	pr, err := newTestGitHub(t, rec).OpenPR(context.Background(), "acme/shop", PRRequest{
		Title: "pg_sage: add index", Body: "why", Branch: "pg_sage/cand-1",
		Labels: []string{"pg_sage"},
		Files:  []FileChange{{Path: "db/migrate/20261115_idx.sql", Content: "CREATE INDEX"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1350, pr.Number)
	require.Equal(t, []string{"pg_sage"}, pr.Labels)
	require.Equal(t, []string{
		"GET " + ghRepoPath,
		"GET " + ghRepoPath + "/git/ref/heads/main",
		"POST " + ghRepoPath + "/git/refs",
		"GET " + ghRepoPath + "/contents/db/migrate/20261115_idx.sql",
		"PUT " + ghRepoPath + "/contents/db/migrate/20261115_idx.sql",
		"POST " + ghRepoPath + "/pulls",
		"POST " + ghRepoPath + "/issues/1350/labels",
	}, rec.keys())
	reqs := rec.requests()
	var ref, put, pull map[string]string
	require.NoError(t, json.Unmarshal([]byte(reqs[2].Body), &ref))
	require.Equal(t, "refs/heads/pg_sage/cand-1", ref["ref"])
	require.Equal(t, "aa218f56b14c9653891f9e74264a383fa43fefbd", ref["sha"])
	require.NoError(t, json.Unmarshal([]byte(reqs[4].Body), &put))
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("CREATE INDEX")), put["content"])
	require.Equal(t, "pg_sage/cand-1", put["branch"])
	require.Equal(t, "", put["sha"], "a new file has no blob sha")
	require.NoError(t, json.Unmarshal([]byte(reqs[5].Body), &pull))
	require.Equal(t, map[string]string{"title": "pg_sage: add index", "body": "why",
		"head": "pg_sage/cand-1", "base": "main"}, pull)
}

func TestGitHubPRStateAndComment(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/pulls/1347":            {fixture: "github/pull_merged.json"},
		"GET " + ghRepoPath + "/pulls/1":               {body: `{"number":1,"state":"closed"}`},
		"GET " + ghRepoPath + "/pulls/2":               {body: `{"number":2,"state":"open"}`},
		"GET " + ghRepoPath + "/pulls/3":               {body: `{"number":3,"state":"weird"}`},
		"POST " + ghRepoPath + "/issues/1347/comments": {status: 201, body: `{"id":1}`},
	})
	gh := newTestGitHub(t, rec)
	ctx := context.Background()
	st, err := gh.PRState(ctx, "acme/shop", 1347)
	require.NoError(t, err)
	require.Equal(t, PRMerged, st.State)
	require.NotNil(t, st.MergedAt)
	require.Equal(t, time.Date(2026, 11, 10, 14, 21, 7, 0, time.UTC), st.MergedAt.UTC())
	st, err = gh.PRState(ctx, "acme/shop", 1)
	require.NoError(t, err)
	require.Equal(t, PRClosed, st.State)
	st, err = gh.PRState(ctx, "acme/shop", 2)
	require.NoError(t, err)
	require.Equal(t, PROpen, st.State)
	_, err = gh.PRState(ctx, "acme/shop", 3)
	require.ErrorIs(t, err, ErrUnavailable)
	require.NoError(t, gh.Comment(ctx, "acme/shop", 1347, "measured: improved"))
	last := rec.requests()[len(rec.requests())-1]
	require.JSONEq(t, `{"body":"measured: improved"}`, last.Body)
}

func TestGitHubStatusErrorsAreDistinguishable(t *testing.T) {
	reset := time.Now().Add(90 * time.Second).Unix()
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/pulls/1": {status: 401, body: `{"message":"Bad credentials"}`},
		"GET " + ghRepoPath + "/pulls/2": {status: 500, body: `oops`},
		"POST " + ghRepoPath + "/issues/3/comments": {status: 403,
			body: `{"message":"Must have admin rights"}`},
	})
	gh := newTestGitHub(t, rec)
	ctx := context.Background()
	_, err := gh.PRState(ctx, "acme/shop", 1)
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, "token cannot read acme/shop (HTTP 401)")
	require.ErrorContains(t, err, "Bad credentials")
	_, err = gh.PRState(ctx, "acme/shop", 2)
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorContains(t, err, "HTTP 500")
	err = gh.Comment(ctx, "acme/shop", 3, "x")
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, "token cannot write to acme/shop")
	_, err = gh.PRState(ctx, "acme/shop", 4)
	require.ErrorIs(t, err, ErrNotFound)
	rec.set("GET "+ghRepoPath+"/pulls/5", answer{status: 403, body: `{}`,
		header: map[string]string{"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset": jsonInt(reset)}})
	_, err = gh.PRState(ctx, "acme/shop", 5)
	var rl *RateLimitError
	require.True(t, errors.As(err, &rl), "quota exhausted is a rate limit: %v", err)
	require.False(t, errors.Is(err, ErrForbidden))
	require.InDelta(t, 90, rl.RetryAfter.Seconds(), 2)
}

func jsonInt(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestGitHubRejectsInvalidInputBeforeAnyRequest(t *testing.T) {
	rec := newRecorder(t, nil)
	gh := newTestGitHub(t, rec)
	ctx := context.Background()
	good := PRRequest{Title: "t", Branch: "pg_sage/x", Files: []FileChange{{Path: "a.sql"}}}
	checks := map[string]error{}
	_, checks["nested repo"] = gh.GetFile(ctx, "acme/backend/shop", "a.rb")
	_, checks["dot dot path"] = gh.GetFile(ctx, "acme/shop", "a/../../b.rb")
	_, checks["absolute path"] = gh.GetFile(ctx, "acme/shop", "/etc/passwd")
	_, checks["empty query"] = gh.SearchCode(ctx, "acme/shop", "  ")
	_, checks["long query"] = gh.SearchCode(ctx, "acme/shop", strings.Repeat("q", 300))
	_, checks["comma label"] = gh.ListOpenPRs(ctx, "acme/shop", "a,b")
	_, checks["zero number"] = gh.PRState(ctx, "acme/shop", 0)
	checks["empty comment"] = gh.Comment(ctx, "acme/shop", 1, " ")
	for name, mutate := range map[string]func(*PRRequest){
		"no title":      func(r *PRRequest) { r.Title = "" },
		"dash branch":   func(r *PRRequest) { r.Branch = "-x" },
		"dotdot branch": func(r *PRRequest) { r.Branch = "a..b" },
		"base = head":   func(r *PRRequest) { r.Base = r.Branch },
		"no files":      func(r *PRRequest) { r.Files = nil },
		"bad file":      func(r *PRRequest) { r.Files = []FileChange{{Path: "../x.sql"}} },
		"bad label":     func(r *PRRequest) { r.Labels = []string{""} },
	} {
		req := good
		mutate(&req)
		_, checks[name] = gh.OpenPR(ctx, "acme/shop", req)
	}
	for name, err := range checks {
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	require.Empty(t, rec.requests(), "validation runs before any request")
}
