package codehosttest

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: every method holds the fake's one mutex.

func TestFakeFilesSearchAndErrors(t *testing.T) {
	f := New()
	ctx := context.Background()
	f.AddFile("a/b", "x.rb", "def index orders")
	f.AddFile("a/b", "y.rb", "orders only")
	hits, err := f.SearchCode(ctx, "a/b", `"orders" index`)
	require.NoError(t, err)
	require.Equal(t, []codehost.SearchHit{{Path: "x.rb"}}, hits)
	hits, err = f.SearchCode(ctx, "a/b", "  ")
	require.NoError(t, err)
	require.Empty(t, hits, "an empty query matches nothing")
	file, err := f.GetFile(ctx, "a/b", "y.rb")
	require.NoError(t, err)
	require.Equal(t, "orders only", file.Content)
	_, err = f.GetFile(ctx, "a/b", "z.rb")
	require.ErrorIs(t, err, codehost.ErrNotFound)
	boom := errors.New("boom")
	f.Fail(boom)
	_, err = f.GetFile(ctx, "a/b", "y.rb")
	require.ErrorIs(t, err, boom)
	f.Fail(nil)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.SearchCode(cancelled, "a/b", "orders")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 6, f.Calls())
}

func TestFakePullRequestLifecycle(t *testing.T) {
	f := New()
	ctx := context.Background()
	pr, err := f.OpenPR(ctx, "a/b", codehost.PRRequest{Title: "t", Body: "<!-- k -->",
		Labels: []string{"pg_sage"}})
	require.NoError(t, err)
	require.Equal(t, 1, pr.Number)
	open, err := f.ListOpenPRs(ctx, "a/b", "pg_sage")
	require.NoError(t, err)
	require.Len(t, open, 1)
	other, err := f.ListOpenPRs(ctx, "a/b", "other")
	require.NoError(t, err)
	require.Empty(t, other)
	st, err := f.PRState(ctx, "a/b", 1)
	require.NoError(t, err)
	require.Equal(t, codehost.PROpen, st.State)
	f.SetState("a/b", codehost.PRStatus{Number: 1, State: codehost.PRMerged})
	st, err = f.PRState(ctx, "a/b", 1)
	require.NoError(t, err)
	require.Equal(t, codehost.PRMerged, st.State)
	_, err = f.PRState(ctx, "a/b", 2)
	require.ErrorIs(t, err, codehost.ErrNotFound)
	require.NoError(t, f.Comment(ctx, "a/b", 1, "note"))
	require.Equal(t, []string{"note"}, f.Comments())
	require.Len(t, f.Opened(), 1)
	require.Equal(t, "fake codehost", f.Name())
}
