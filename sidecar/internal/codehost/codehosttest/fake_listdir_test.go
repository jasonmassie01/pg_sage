package codehosttest

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestFakeListDirListsDirectChildrenAndSeesOpenedFiles(t *testing.T) {
	f := New()
	ctx := context.Background()
	f.AddFile("a/b", "db/migrate/0002_b.sql", "b")
	f.AddFile("a/b", "db/migrate/0001_a.sql", "a")
	f.AddFile("a/b", "db/migrate/old/0000_x.sql", "x")
	f.AddFile("a/b", "db/other.sql", "o")
	names, err := f.ListDir(ctx, "a/b", "db/migrate")
	require.NoError(t, err)
	require.Equal(t, []string{"0001_a.sql", "0002_b.sql"}, names)

	_, err = f.OpenPR(ctx, "a/b", codehost.PRRequest{Title: "t", Branch: "x",
		Files: []codehost.FileChange{{Path: "db/migrate/0003_c.sql", Content: "c"}}})
	require.NoError(t, err)
	names, err = f.ListDir(ctx, "a/b", "db/migrate")
	require.NoError(t, err)
	require.Equal(t, []string{"0001_a.sql", "0002_b.sql"}, names,
		"an open PR's files are on its branch, not the default branch")

	_, err = f.ListDir(ctx, "a/b", "db/none")
	require.ErrorIs(t, err, codehost.ErrNotFound)
	boom := errors.New("boom")
	f.Fail(boom)
	_, err = f.ListDir(ctx, "a/b", "db/migrate")
	require.ErrorIs(t, err, boom)
	require.Equal(t, 5, f.Calls(), "three listings, one open, one failed listing")
}

func TestFakeFailOpenPRFailsOnlyOpenPR(t *testing.T) {
	f := New()
	ctx := context.Background()
	f.AddFile("a/b", "x.sql", "x")
	boom := errors.New("refused")
	f.FailOpenPR(boom)
	_, err := f.OpenPR(ctx, "a/b", codehost.PRRequest{Title: "t", Branch: "x"})
	require.ErrorIs(t, err, boom)
	require.Empty(t, f.Opened())
	_, err = f.GetFile(ctx, "a/b", "x.sql")
	require.NoError(t, err)
	f.FailOpenPR(nil)
	_, err = f.OpenPR(ctx, "a/b", codehost.PRRequest{Title: "t", Branch: "x"})
	require.NoError(t, err)
	require.Equal(t, 1, len(f.Opened()))
}

func TestFakeClosePRDropsItFromTheOpenList(t *testing.T) {
	f := New()
	ctx := context.Background()
	pr, err := f.OpenPR(ctx, "a/b", codehost.PRRequest{Title: "t", Branch: "x",
		Labels: []string{"pg_sage"}})
	require.NoError(t, err)
	f.SetState("a/b", codehost.PRStatus{Number: pr.Number, State: codehost.PRClosed})
	open, err := f.ListOpenPRs(ctx, "a/b", "pg_sage")
	require.NoError(t, err)
	require.Empty(t, open, "a closed or merged PR is not listed as open")
}
