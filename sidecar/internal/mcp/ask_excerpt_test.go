package mcp

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp-05b: ask_sage results carry each citation's excerpt (additive), and
// a citation without one has no excerpt key. No concurrency or state
// tests: the tool returns the backend's answer as it is.

type excerptAskBackend struct{ askRecordingBackend }

func (b *excerptAskBackend) AskSage(_ context.Context, database string, _ ask.Caller,
	r ask.Request) (ask.Answer, error) {
	return ask.Answer{ID: 3, Database: database, Status: ask.StatusAnswered,
		Question: r.Question, Text: "Seq scans on public.orders. [1]",
		Citations: []ask.Citation{
			{ID: "finding:42", Alias: "E1", Kind: "finding", Ref: "42", Label: "finding:42 ok",
				Digest: "aa", Excerpt: "finding 42 [warning, open] Missing index"},
			{ID: "doc:x", Alias: "E2", Kind: "doc", Ref: "x", Label: "doc:x ok",
				Digest: "bb", ExcerptTruncated: true},
			{ID: "config:all", Alias: "E3", Kind: "config", Ref: "all",
				Label: "config:all ok", Digest: "cc"},
		}}, nil
}

func TestAskToolCarriesCitationExcerpts(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:2", Role: "operator"})
	response := invoke(t, NewServer(&excerptAskBackend{}), ctx, toolCall("ask_sage", askArgs))
	require.Empty(t, response.Error.Code)
	content := structuredContent(t, response)
	cites, ok := content["citations"].([]any)
	require.True(t, ok, "citations missing")
	require.Equal(t, 3, len(cites))
	first := objectMap(t, cites[0])
	require.Equal(t, "finding 42 [warning, open] Missing index", first["excerpt"])
	_, hasFlag := first["excerpt_truncated"]
	require.False(t, hasFlag, "a whole excerpt must not claim truncation")
	second := objectMap(t, cites[1])
	require.Equal(t, true, second["excerpt_truncated"])
	_, hasText := second["excerpt"]
	require.False(t, hasText, "an over-budget citation carries no excerpt text")
	third := objectMap(t, cites[2])
	_, hasText = third["excerpt"]
	require.False(t, hasText, "a citation without an excerpt carries no excerpt key")
}
