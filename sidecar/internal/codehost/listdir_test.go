package codehost

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrency tests: ListDir shares only the client's cap and cool-down,
// which rest_test.go and ratelimit_queue_test.go cover.

func TestGitHubListDirReturnsFilesSortedWithoutSubdirectories(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/contents/db/migrate": {body: `[
		 {"type":"file","name":"20260102030405_add_orders.rb","path":"db/migrate/x"},
		 {"type":"dir","name":"archive","path":"db/migrate/archive"},
		 {"type":"file","name":"20250101000000_init.rb","path":"db/migrate/y"}]`}})
	names, err := newTestGitHub(t, rec).ListDir(context.Background(), "acme/shop",
		"db/migrate")
	require.NoError(t, err)
	require.Equal(t, []string{"20250101000000_init.rb", "20260102030405_add_orders.rb"}, names)
	require.Equal(t, []string{"GET " + ghRepoPath + "/contents/db/migrate"}, rec.keys())
}

func TestGitHubListDirOnAFileIsNotFound(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/contents/db/schema.rb": {fixture: "github/contents_file.json"}})
	_, err := newTestGitHub(t, rec).ListDir(context.Background(), "acme/shop", "db/schema.rb")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "db/schema.rb in acme/shop is not a directory")
}

func TestListDirRefusesAnUnsafePathBeforeAnyRequest(t *testing.T) {
	rec := newRecorder(t, nil)
	gh := newTestGitHub(t, rec)
	gl := newTestGitLab(t, rec)
	for _, dir := range []string{"", "../etc", "db/../..", "/abs", `db\migrate`, "C:/x"} {
		_, err := gh.ListDir(context.Background(), "acme/shop", dir)
		require.ErrorIs(t, err, ErrInvalid, "github %q", dir)
		_, err = gl.ListDir(context.Background(), "acme/backend/shop", dir)
		require.ErrorIs(t, err, ErrInvalid, "gitlab %q", dir)
	}
	require.Equal(t, 0, len(rec.requests()), "no request for an unsafe path")
}

func TestGitHubListDirMissingDirectoryIsNotFound(t *testing.T) {
	rec := newRecorder(t, nil)
	_, err := newTestGitHub(t, rec).ListDir(context.Background(), "acme/shop", "db/none")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGitLabListDirFollowsPagesAndKeepsOnlyBlobs(t *testing.T) {
	var page1 []string
	for i := 0; i < glTreePage; i++ {
		page1 = append(page1, fmt.Sprintf(`{"type":"blob","name":"%04d_m.sql"}`, i))
	}
	page1[3] = `{"type":"tree","name":"sub"}`
	rec := newRecorder(t, nil)
	rec.fallback = answer{body: "[" + strings.Join(page1, ",") + "]"}
	path := "GET /api/v4" + glProjectPath + "/repository/tree"
	rec.set(path, answer{body: "[" + strings.Join(page1, ",") + "]"})
	gl := newTestGitLab(t, rec)
	// The recorder keys by path only, so it answers every page alike; the
	// page cap bounds the walk.
	names, err := gl.ListDir(context.Background(), "acme/backend/shop", "db/migrations")
	require.ErrorIs(t, err, ErrTooLarge, "a directory over the cap is refused, not cut")
	require.Nil(t, names)
	require.Equal(t, glTreeMaxPages, len(rec.requests()))
	q, _ := url.ParseQuery(rec.requests()[1].Query)
	require.Equal(t, "db/migrations", q.Get("path"))
	require.Equal(t, "2", q.Get("page"))
	require.Equal(t, fmt.Sprint(glTreePage), q.Get("per_page"))
}

func TestGitLabListDirShortPageEndsTheWalk(t *testing.T) {
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath + "/repository/tree": {body: `[
		 {"type":"blob","name":"0002_b.sql"},{"type":"tree","name":"old"},
		 {"type":"blob","name":"0001_a.sql"}]`}})
	names, err := newTestGitLab(t, rec).ListDir(context.Background(), "acme/backend/shop",
		"db/migrations")
	require.NoError(t, err)
	require.Equal(t, []string{"0001_a.sql", "0002_b.sql"}, names)
	require.Equal(t, 1, len(rec.requests()))
	require.Equal(t, testToken, rec.requests()[0].Header.Get("PRIVATE-TOKEN"))
}

func TestGitLabListDirEmptyIsNotFound(t *testing.T) {
	// GitLab answers an unknown path in the tree with an empty list.
	rec := newRecorder(t, map[string]answer{
		"GET /api/v4" + glProjectPath + "/repository/tree": {body: `[]`}})
	_, err := newTestGitLab(t, rec).ListDir(context.Background(), "acme/backend/shop", "db/x")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "db/x in acme/backend/shop")
}
