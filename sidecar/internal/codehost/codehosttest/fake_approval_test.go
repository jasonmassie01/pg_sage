package codehosttest

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// cli-02b: the fake merges and closes a PR the way a person would on the
// host, and the merged files are readable at the merge commit.

func TestFakeMergeWritesTheFilesAndRecordsTheMerger(t *testing.T) {
	f := New()
	ctx := context.Background()
	pr, err := f.OpenPR(ctx, "o/cfg", codehost.PRRequest{Title: "t", Branch: "b",
		Files: []codehost.FileChange{{Path: "approvals/db/1.yaml", Content: "v1"}}})
	require.NoError(t, err)
	_, err = f.GetFileAt(ctx, "o/cfg", "approvals/db/1.yaml", "x")
	require.ErrorIs(t, err, codehost.ErrNotFound, "an unmerged PR's file is not on the base")
	f.Merge("o/cfg", pr.Number, "octo", nil)
	st, err := f.PRState(ctx, "o/cfg", pr.Number)
	require.NoError(t, err)
	require.Equal(t, codehost.PRMerged, st.State)
	require.Equal(t, "octo", st.MergedBy)
	require.NotEqual(t, "", st.MergeSHA)
	file, err := f.GetFileAt(ctx, "o/cfg", "approvals/db/1.yaml", st.MergeSHA)
	require.NoError(t, err)
	require.Equal(t, "v1", file.Content)
	open, err := f.ListOpenPRs(ctx, "o/cfg", "")
	require.NoError(t, err)
	require.Empty(t, open)
}

func TestFakeMergeCanEditTheFileAndClosePRCloses(t *testing.T) {
	f := New()
	ctx := context.Background()
	pr, err := f.OpenPR(ctx, "o/cfg", codehost.PRRequest{Title: "t", Branch: "b",
		Files: []codehost.FileChange{{Path: "a.yaml", Content: "sql: SELECT 1"}}})
	require.NoError(t, err)
	f.Merge("o/cfg", pr.Number, "octo", func(path, content string) string {
		return content + "; DROP TABLE x"
	})
	file, err := f.GetFileAt(ctx, "o/cfg", "a.yaml", "")
	require.NoError(t, err)
	require.Equal(t, "sql: SELECT 1; DROP TABLE x", file.Content)
	pr2, err := f.OpenPR(ctx, "o/cfg", codehost.PRRequest{Title: "t2", Branch: "b2"})
	require.NoError(t, err)
	require.NoError(t, f.ClosePR(ctx, "o/cfg", pr2.Number))
	st, err := f.PRState(ctx, "o/cfg", pr2.Number)
	require.NoError(t, err)
	require.Equal(t, codehost.PRClosed, st.State)
	require.Equal(t, []int{pr2.Number}, f.Closed())
	require.ErrorIs(t, f.ClosePR(ctx, "o/cfg", 99), codehost.ErrNotFound)
}
