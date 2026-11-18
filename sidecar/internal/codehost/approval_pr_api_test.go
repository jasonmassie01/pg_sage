package codehost

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// cli-02b: an approval PR needs the merger's login and merge commit, the
// merged file at that commit, and a way to close a PR pg_sage opened.

func TestGitHubPRStateReportsMergerAndMergeCommit(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/pulls/9": {body: `{"number":9,"state":"closed",` +
			`"merged":true,"merged_at":"2026-11-10T14:21:07Z",` +
			`"merged_by":{"login":"octo-dba"},"merge_commit_sha":"abc123"}`},
		"GET " + ghRepoPath + "/pulls/10": {body: `{"number":10,"state":"open"}`},
	})
	gh := newTestGitHub(t, rec)
	st, err := gh.PRState(context.Background(), "acme/shop", 9)
	require.NoError(t, err)
	require.Equal(t, PRMerged, st.State)
	require.Equal(t, "octo-dba", st.MergedBy)
	require.Equal(t, "abc123", st.MergeSHA)
	st, err = gh.PRState(context.Background(), "acme/shop", 10)
	require.NoError(t, err)
	require.Equal(t, PROpen, st.State)
	require.Equal(t, "", st.MergedBy, "an open PR has no merger")
}

func TestGitHubGetFileAtReadsTheGivenRef(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/contents/approvals/db1/7.yaml": {body: `{"type":"file",` +
			`"encoding":"base64","content":"a2luZDogeAo=","size":8,"sha":"s1"}`},
	})
	gh := newTestGitHub(t, rec)
	f, err := gh.GetFileAt(context.Background(), "acme/shop", "approvals/db1/7.yaml", "abc123")
	require.NoError(t, err)
	require.Equal(t, "kind: x\n", f.Content)
	reqs := rec.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "ref=abc123", reqs[0].Query)
}

func TestGitHubClosePRPatchesStateClosed(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"PATCH " + ghRepoPath + "/pulls/4": {body: `{"number":4,"state":"closed"}`},
	})
	gh := newTestGitHub(t, rec)
	require.NoError(t, gh.ClosePR(context.Background(), "acme/shop", 4))
	reqs := rec.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, http.MethodPatch, reqs[0].Method)
	require.JSONEq(t, `{"state":"closed"}`, reqs[0].Body)
}

func TestGitLabPRStateReportsMergerAndMergeCommit(t *testing.T) {
	mr := "/api/v4" + glProjectPath + "/merge_requests/"
	rec := newRecorder(t, map[string]answer{
		"GET " + mr + "21": {body: `{"iid":21,"state":"merged",` +
			`"merged_at":"2026-11-10T14:21:07Z","merged_by":{"username":"gl-dba"},` +
			`"merge_commit_sha":"def456"}`},
		"GET " + mr + "22": {body: `{"iid":22,"state":"merged","merged_by":` +
			`{"username":"gl-dba"},"merge_commit_sha":null,"squash_commit_sha":"sq789"}`},
	})
	gl := newTestGitLab(t, rec)
	st, err := gl.PRState(context.Background(), "acme/backend/shop", 21)
	require.NoError(t, err)
	require.Equal(t, PRMerged, st.State)
	require.Equal(t, "gl-dba", st.MergedBy)
	require.Equal(t, "def456", st.MergeSHA)
	st, err = gl.PRState(context.Background(), "acme/backend/shop", 22)
	require.NoError(t, err)
	require.Equal(t, "sq789", st.MergeSHA, "a squash merge is read at its squash commit")
}

func TestGitLabGetFileAtAndClosePR(t *testing.T) {
	base := "/api/v4" + glProjectPath
	rec := newRecorder(t, map[string]answer{
		"GET " + base + "/repository/files/approvals%2Fdb1%2F7.yaml/raw": {body: "kind: y\n"},
		"PUT " + base + "/merge_requests/5": {body: `{"iid":5,"state":"closed"}`},
	})
	gl := newTestGitLab(t, rec)
	f, err := gl.GetFileAt(context.Background(), "acme/backend/shop", "approvals/db1/7.yaml",
		"def456")
	require.NoError(t, err)
	require.Equal(t, "kind: y\n", f.Content)
	require.NoError(t, gl.ClosePR(context.Background(), "acme/backend/shop", 5))
	reqs := rec.requests()
	require.Len(t, reqs, 2)
	require.Equal(t, "ref=def456", reqs[0].Query)
	require.Equal(t, http.MethodPut, reqs[1].Method)
	require.JSONEq(t, `{"state_event":"close"}`, reqs[1].Body)
}

func TestGetFileAtAndClosePRRefuseBadInputBeforeSending(t *testing.T) {
	rec := newRecorder(t, nil)
	gh, gl := newTestGitHub(t, rec), newTestGitLab(t, rec)
	ctx := context.Background()
	for _, ref := range []string{"", "../main", "-x", "a b"} {
		_, err := gh.GetFileAt(ctx, "acme/shop", "a.yaml", ref)
		require.ErrorIs(t, err, ErrInvalid)
		_, err = gl.GetFileAt(ctx, "acme/backend/shop", "a.yaml", ref)
		require.ErrorIs(t, err, ErrInvalid)
	}
	require.ErrorIs(t, gh.ClosePR(ctx, "acme/shop", 0), ErrInvalid)
	require.ErrorIs(t, gl.ClosePR(ctx, "acme/backend/shop", -1), ErrInvalid)
	require.ErrorIs(t, gh.ClosePR(ctx, "acme", 1), ErrInvalid)
	require.Empty(t, rec.requests(), "nothing is sent for a refused request")
}

func TestClosePRErrorsCarryNoToken(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"PATCH " + ghRepoPath + "/pulls/4": {status: 401,
			body: `{"message":"Bad credentials ` + testToken + `"}`},
		"PATCH " + ghRepoPath + "/pulls/5": {status: 502, body: `bad gateway`},
	})
	gh := newTestGitHub(t, rec)
	err := gh.ClosePR(context.Background(), "acme/shop", 4)
	require.ErrorIs(t, err, ErrForbidden)
	require.False(t, strings.Contains(err.Error(), testToken), "token leaked: %v", err)
	err = gh.ClosePR(context.Background(), "acme/shop", 5)
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorContains(t, err, "HTTP 502")
}
