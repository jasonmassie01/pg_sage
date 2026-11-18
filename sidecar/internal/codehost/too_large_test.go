package codehost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// A file too large to read is ErrTooLarge (and still ErrUnavailable) on
// both hosts, so a caller can skip it; an unexpected encoding is not.
func TestFileTooLargeIsDistinguishableOnBothHosts(t *testing.T) {
	big := strings.Repeat("x", MaxFileBytes+1)
	huge := strings.Repeat("y", maxResponseBytes+1)
	files := "/api/v4" + glProjectPath + "/repository/files/"
	gl := newTestGitLab(t, newRecorder(t, map[string]answer{
		"GET " + files + "big.sql/raw":  {body: big},
		"GET " + files + "huge.sql/raw": {body: huge},
		"GET " + files + "ok.sql/raw":   {body: "SELECT 1\n"},
	}))
	ctx := context.Background()
	for _, path := range []string{"big.sql", "huge.sql"} {
		_, err := gl.GetFile(ctx, "acme/backend/shop", path)
		require.ErrorIs(t, err, ErrTooLarge, path)
		require.ErrorIs(t, err, ErrUnavailable, path)
	}
	f, err := gl.GetFile(ctx, "acme/backend/shop", "ok.sql")
	require.NoError(t, err)
	require.Equal(t, "SELECT 1\n", f.Content)

	gh := newTestGitHub(t, newRecorder(t, map[string]answer{
		"GET " + ghRepoPath + "/contents/big.sql": {body: `{"type":"file",` +
			`"encoding":"none","size":5242880,"content":""}`},
		"GET " + ghRepoPath + "/contents/odd.sql": {body: `{"type":"file",` +
			`"encoding":"utf-16","size":10,"content":"x"}`},
	}))
	_, err = gh.GetFile(ctx, "acme/shop", "big.sql")
	require.ErrorIs(t, err, ErrTooLarge)
	_, err = gh.GetFile(ctx, "acme/shop", "odd.sql")
	require.ErrorIs(t, err, ErrUnavailable)
	require.False(t, errors.Is(err, ErrTooLarge), "an odd encoding is not a size: %v", err)
	require.ErrorContains(t, err, `unexpected encoding "utf-16"`)
}
