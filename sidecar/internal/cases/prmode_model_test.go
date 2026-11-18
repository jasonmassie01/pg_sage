package cases

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: the paragraph is built per call from its
// arguments; the model double is not shared between goroutines.

const goodParagraph = "This adds one migration that checks orders.total is never negative. " +
	"It does not scan existing rows."

var errRateLimited = errors.New("llm: HTTP 429 Too Many Requests")

func TestPRModeModelParagraphFallsBackToTheTemplate(t *testing.T) {
	for name, model := range map[string]*stubModel{
		"malformed json": {reply: `{"paragraph": "unterminated`},
		"empty":          {reply: ""},
		"empty field":    {reply: `{"paragraph": "  "}`},
		"wrong shape":    {reply: `["a"]`},
		"timeout":        {err: context.DeadlineExceeded},
		"rate limited":   {err: errRateLimited},
		"url":            {reply: `{"paragraph": "See https://evil.example/x for details."}`},
		"unknown identifier": {reply: `{"paragraph": "This also rebuilds ` +
			`billing.invoices_idx for speed."}`},
		"too long": {reply: `{"paragraph": "` + strings.Repeat("word ", 400) + `"}`},
	} {
		host, rec := migrationRepo(), newMemRecords()
		out, err := newTestPRMode(host, rec, model, nil).Run(context.Background(),
			migrationCandidate())
		require.NoError(t, err, name)
		require.Equal(t, PROpened, out.Status, name)
		body := host.Opened()[0].Body
		require.Contains(t, body, "This adds one new migration file", name)
		require.Equal(t, 1, model.calls, name)
	}
}

func TestPRModeModelParagraphIsUsedWhenItParses(t *testing.T) {
	for name, reply := range map[string]string{
		"plain":  `{"paragraph": "` + goodParagraph + `"}`,
		"fenced": "```json\n{\"paragraph\": \"" + goodParagraph + "\"}\n```",
		"prose":  "Here you go:\n{\"paragraph\": \"" + goodParagraph + "\"}",
	} {
		host, rec := migrationRepo(), newMemRecords()
		model := &stubModel{reply: reply}
		_, err := newTestPRMode(host, rec, model, nil).Run(context.Background(),
			migrationCandidate())
		require.NoError(t, err, name)
		body := host.Opened()[0].Body
		require.Contains(t, body, goodParagraph, name)
		require.NotContains(t, body, "This adds one new migration file", name)
	}
}

func TestPRModeModelSeesOnlyRedactedFacts(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	model := &stubModel{reply: `{"paragraph": "` + goodParagraph + `"}`}
	c := migrationCandidate()
	c.MigrationSQL = "UPDATE accounts SET note = 'ssn 123-45-6789' WHERE id = 1;"
	_, err := newTestPRMode(host, rec, model, nil).Run(context.Background(), c)
	require.NoError(t, err)
	require.NotContains(t, model.user, "123-45-6789", "no SQL literal reaches the model")
	require.NotContains(t, model.user, c.Why, "no free text beyond the facts")
	require.Contains(t, model.user, `"kind":"migration"`)
	require.Contains(t, model.user, `"risk_tier":"high"`)
}

func TestParagraphIdentifierCheck(t *testing.T) {
	facts := `{"title":"Add a NOT VALID check on orders.total","files":["db/m/0002_x.sql"]}`
	ok := []string{"It adds a check on orders.total.", "It writes db/m/0002_x.sql."}
	bad := []string{"It touches users.email.", "It renames total_amount.", "See www.x.io",
		"It mentions #1234 and PR 98765."}
	for _, p := range ok {
		require.True(t, paragraphGrounded(p, facts), p)
	}
	for _, p := range bad {
		require.False(t, paragraphGrounded(p, facts), p)
	}
}
