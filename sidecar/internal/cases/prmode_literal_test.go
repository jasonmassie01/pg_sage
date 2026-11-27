package cases

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: replaceLiteral is a pure function.

// A rewrite must not add characters the host language may read inside the
// literal (interpolation, format verbs, comments) or a bind placeholder.
func TestReplaceLiteralRefusesHostLanguageSyntax(t *testing.T) {
	stmt := "SELECT id FROM orders WHERE status = 'open' ORDER BY id"
	for name, rewrite := range map[string]string{
		"interpolation": stmt + " LIMIT ${n}",
		"brace":         stmt + " LIMIT {n}",
		"format verb":   "SELECT id FROM orders WHERE status = 'open' AND ref LIKE 'a%'",
		"hash":          stmt + " LIMIT #{n}",
		"placeholder":   stmt + " LIMIT ?",
	} {
		_, err := replaceLiteral(appFile, 3, stmt, rewrite)
		require.ErrorIs(t, err, errNoPR, name)
	}
	params := "SELECT id FROM orders WHERE status = $1 ORDER BY id"
	content := "package x\n\nconst q = \"" + params + "\"\n"
	got, err := replaceLiteral(content, 3, params, params+" LIMIT 500")
	require.NoError(t, err, "a $1 the statement already has is kept")
	require.Contains(t, got, params+" LIMIT 500")
}
