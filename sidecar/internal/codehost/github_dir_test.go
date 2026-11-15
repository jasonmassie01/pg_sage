package codehost

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrency tests: one request, no shared state beyond the client's.

func TestGitHubGetFileOnADirectoryIsNotFoundLikeGitLab(t *testing.T) {
	// The contents API answers a directory with a JSON array. A file tag
	// that names a directory is "no such file" (GitLab's raw endpoint says
	// 404), so the finder falls through instead of failing the lookup.
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/contents/app/models": {fixture: "github/contents_dir.json"},
		"GET " + ghRepoPath + "/contents/app/odd.rb": {body: `{"type":`},
	})
	gh := newTestGitHub(t, rec)
	_, err := gh.GetFile(context.Background(), "acme/shop", "app/models")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "app/models in acme/shop is a dir, not a file")
	_, err = gh.GetFile(context.Background(), "acme/shop", "app/odd.rb")
	require.ErrorIs(t, err, ErrUnavailable, "a truncated answer is still an error")
	require.ErrorContains(t, err, "unexpected answer")
}
