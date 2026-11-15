package agenttools

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: codeLiterals and searchTerms are pure.

const pythonSource = `# don't match this apostrophe
def open_orders(cur):
    cur.execute("""
        SELECT id FROM orders WHERE status = %(status)s
    """, {"status": "open"})
    cur.execute('DELETE FROM carts WHERE id = %s', (1,))
    name = "not sql"
    bad = "SELECT unterminated
`

func TestCodeLiteralsAcrossLanguages(t *testing.T) {
	got := codeLiterals(pythonSource)
	require.Len(t, got, 2, "%+v", got)
	require.Equal(t, 3, got[0].line)
	require.Equal(t, sqlFingerprint("SELECT id FROM orders WHERE status = $1"),
		sqlFingerprint(got[0].text))
	require.Equal(t, 6, got[1].line)
	require.Equal(t, "DELETE FROM carts WHERE id = %s", got[1].text)

	ruby := "x = 1 << 2\nsql = <<-'SQL'\n  UPDATE t SET a = 1\n  SQL\nputs <<~NOPE\nselect\n"
	got = codeLiterals(ruby)
	require.Len(t, got, 1, "an unterminated heredoc is not a literal: %+v", got)
	require.Equal(t, 2, got[0].line)
	require.Equal(t, "update t set a = ?", sqlFingerprint(got[0].text))

	escaped := `q := "SELECT \"Name\" FROM people\nWHERE id = $1"`
	got = codeLiterals(escaped)
	require.Len(t, got, 1)
	require.Equal(t, sqlFingerprint(`SELECT "Name" FROM people WHERE id = $1`),
		sqlFingerprint(got[0].text))
	require.Empty(t, codeLiterals(""))
	require.Empty(t, codeLiterals("no literals here at all"))
}

func TestSearchTermsPickLongestIdentifiers(t *testing.T) {
	require.Equal(t, []string{"customer_id", "orders", "status"},
		searchTerms("SELECT id, total FROM orders WHERE customer_id = $1 AND status = $2"))
	require.Empty(t, searchTerms("SELECT count(*) FROM t WHERE a = $1"), "short names only")
	require.Empty(t, searchTerms(""))
	require.Empty(t, searchTerms("/* x */"))
	require.Equal(t, []string{"orderlines", "qty"},
		searchTerms(`SELECT "OrderLines".qty FROM "OrderLines"`), "quoted names, once")
}
