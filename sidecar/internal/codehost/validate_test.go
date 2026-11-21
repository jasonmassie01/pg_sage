package codehost

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: the validators are pure functions.

func TestValidatePath(t *testing.T) {
	for _, ok := range []string{"a.rb", "app/models/order.rb", "db/migrate/2026_x.sql",
		".github/workflows/ci.yml", "a..b/c.rb"} {
		require.NoError(t, ValidatePath(ok), ok)
	}
	for _, bad := range []string{"", "/etc/passwd", `C:\x.rb`, "c:/x.rb", `a\b.rb`, "..",
		"../a.rb", "a/../b.rb", "a/./b.rb", "a//b.rb", "a/", "a\x00b", "a\nb",
		strings.Repeat("a", 1025)} {
		require.ErrorIs(t, ValidatePath(bad), ErrInvalid, "%q", bad)
	}
}

func TestValidateRepo(t *testing.T) {
	for _, ok := range []string{"acme/shop", "acme/backend/shop", "a-b/c.d_e"} {
		require.NoError(t, ValidateRepo(ok), ok)
	}
	for _, bad := range []string{"", "acme", "/acme/shop", "acme/shop/", "acme/../x",
		"acme/./x", "acme/sh op", "acme/shop?x", "acme%2Fshop"} {
		require.ErrorIs(t, ValidateRepo(bad), ErrInvalid, "%q", bad)
	}
}

func TestValidBranchAndLabel(t *testing.T) {
	require.NoError(t, validBranch("pg_sage/cand-1"))
	for _, bad := range []string{"", "-x", "/x", "x/", "a..b", "a b", "x.lock",
		strings.Repeat("b", 201)} {
		require.ErrorIs(t, validBranch(bad), ErrInvalid, "%q", bad)
	}
	require.NoError(t, validLabel("pg_sage"))
	for _, bad := range []string{"", " ", "a,b", "a\nb", strings.Repeat("l", 51)} {
		require.ErrorIs(t, validLabel(bad), ErrInvalid, "%q", bad)
	}
}
