package ask

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// mcp-05b: a citation carries an excerpt of what the model read, so an
// operator can audit it later. Each excerpt is the evidence text, redacted
// again and clipped to MaxExcerptRunes runes; one answer's excerpts share
// a MaxExcerptBytes budget, earlier citations first. Unit tests: no
// database, no model.
// No error-propagation tests: excerpts are copied from the in-memory
// result; nothing here can fail.
// No concurrency tests: compose is a pure function run in one goroutine.
// No state-transition tests: a citation has no lifecycle of its own.

// excerptResult is a concluded run whose one claim cites every alias of
// evs, in order (E1, E2, ...).
func excerptResult(evs ...agentloop.Evidence) agentloop.Result {
	issued := map[string]agentloop.Evidence{}
	var aliases []string
	for i, e := range evs {
		alias := fmt.Sprintf("E%d", i+1)
		issued[alias] = e
		aliases = append(aliases, alias)
	}
	return agentloop.Result{Final: json.RawMessage(`{"claims":[]}`),
		Claims:   []agentloop.Claim{{Text: "The reads say so.", Aliases: aliases}},
		Evidence: issued, Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}
}

func TestCompose_CitationCarriesTheTextTheModelRead(t *testing.T) {
	text := "finding:42 [warning, open] Missing index on public.orders | seq scans 4200"
	a := compose("testdb", excerptResult(ev("finding:42", text)), nil)
	if len(a.Citations) != 1 {
		t.Fatalf("citations = %+v", a.Citations)
	}
	c := a.Citations[0]
	if c.Excerpt != text || c.ExcerptTruncated {
		t.Fatalf("excerpt = %q truncated %v, want the evidence text whole", c.Excerpt,
			c.ExcerptTruncated)
	}
	if c.Digest != digestOf(text) || c.Alias != "E1" || len(a.Statements) != 1 {
		t.Fatalf("citation = %+v statements %+v: the excerpt changed grounding", c,
			a.Statements)
	}
}

func TestCompose_ExcerptKeepsExactlyMaxRunes(t *testing.T) {
	// Boundary: runes, not bytes: "é" is two bytes.
	whole := strings.Repeat("é", MaxExcerptRunes)
	a := compose("testdb", excerptResult(ev("doc:a", whole)), nil)
	if c := a.Citations[0]; c.Excerpt != whole || c.ExcerptTruncated {
		t.Fatalf("1500 runes: %d runes, truncated %v, want kept whole",
			utf8.RuneCountInString(c.Excerpt), c.ExcerptTruncated)
	}
	over := whole + "z"
	a = compose("testdb", excerptResult(ev("doc:b", over)), nil)
	c := a.Citations[0]
	if c.Excerpt != whole || !c.ExcerptTruncated {
		t.Fatalf("1501 runes: %d runes (ends %q), truncated %v, want 1500 and truncated",
			utf8.RuneCountInString(c.Excerpt), c.Excerpt[len(c.Excerpt)-2:],
			c.ExcerptTruncated)
	}
	if c.Digest != digestOf(over) {
		t.Fatalf("digest = %s, want the digest of the full text the model saw", c.Digest)
	}
}

// fill is n evidence items of size ASCII letters each.
func fill(n, size int) []agentloop.Evidence {
	var out []agentloop.Evidence
	for i := 0; i < n; i++ {
		letter := string(rune('a' + i%26))
		out = append(out, ev(fmt.Sprintf("doc:%d", i), strings.Repeat(letter, size)))
	}
	return out
}

func TestCompose_ExcerptBudgetFillsThenTruncatesLaterCitations(t *testing.T) {
	// 16 x 1500 bytes = 24000 of 24576; the 17th (1500) does not fit.
	a := compose("testdb", excerptResult(fill(20, MaxExcerptRunes)...), nil)
	if len(a.Citations) != 20 {
		t.Fatalf("citations = %d, want 20", len(a.Citations))
	}
	total := 0
	for i, c := range a.Citations {
		total += len(c.Excerpt)
		kept := i < 16
		if kept && (len(c.Excerpt) != MaxExcerptRunes || c.ExcerptTruncated) {
			t.Fatalf("citation %d: %d bytes truncated %v, want kept", i+1, len(c.Excerpt),
				c.ExcerptTruncated)
		}
		if !kept && (c.Excerpt != "" || !c.ExcerptTruncated) {
			t.Fatalf("citation %d: %d bytes truncated %v, want no text and truncated", i+1,
				len(c.Excerpt), c.ExcerptTruncated)
		}
		if c.Digest == "" || c.Label == "" {
			t.Fatalf("citation %d lost its digest or label: %+v", i+1, c)
		}
	}
	if total > MaxExcerptBytes || MaxExcerptBytes != 24*1024 {
		t.Fatalf("excerpts hold %d bytes, budget %d, want <= 24 KiB", total, MaxExcerptBytes)
	}
	raw, err := json.Marshal(a.Citations[16])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"excerpt":`) ||
		!strings.Contains(string(raw), `"excerpt_truncated":true`) {
		t.Fatalf("over-budget citation JSON = %s, want excerpt_truncated and no excerpt", raw)
	}
}

func TestCompose_ExcerptThatExactlyFillsTheBudgetIsKept(t *testing.T) {
	// Boundary: 16 x 1500 + 576 = 24576 bytes fills the budget exactly;
	// a following one-byte excerpt no longer fits.
	evs := append(fill(16, MaxExcerptRunes), ev("doc:last", strings.Repeat("q", 576)),
		ev("doc:after", "x"))
	a := compose("testdb", excerptResult(evs...), nil)
	if c := a.Citations[16]; len(c.Excerpt) != 576 || c.ExcerptTruncated {
		t.Fatalf("budget-filling citation: %d bytes truncated %v, want kept", len(c.Excerpt),
			c.ExcerptTruncated)
	}
	if c := a.Citations[17]; c.Excerpt != "" || !c.ExcerptTruncated {
		t.Fatalf("citation after a full budget: %q truncated %v, want none", c.Excerpt,
			c.ExcerptTruncated)
	}
}

func TestCompose_ExcerptBudgetStaysClosedForLaterSmallCitations(t *testing.T) {
	// Boundary: 16 x 1500 = 24000 bytes leaves 576. The 17th (1500) does
	// not fit and closes the budget, so the small 18th keeps no text even
	// though it would fit: the excerpts kept are always the earliest ones.
	evs := append(fill(17, MaxExcerptRunes), ev("doc:small", "tiny"))
	a := compose("testdb", excerptResult(evs...), nil)
	if c := a.Citations[16]; c.Excerpt != "" || !c.ExcerptTruncated {
		t.Fatalf("17th citation: %d bytes truncated %v, want dropped", len(c.Excerpt),
			c.ExcerptTruncated)
	}
	if c := a.Citations[17]; c.Excerpt != "" || !c.ExcerptTruncated {
		t.Fatalf("18th citation: %q truncated %v, want dropped after the budget closed",
			c.Excerpt, c.ExcerptTruncated)
	}
}

func TestCompose_ExcerptIsRedactedAgain(t *testing.T) {
	// Invalid input: a tool that let a literal through. The excerpt is
	// redacted as defence in depth; the digest still names what was read.
	text := "query 81 | SELECT * FROM api_keys WHERE token = 'secret-token-123' | 9 calls"
	a := compose("testdb", excerptResult(ev("queries:calls", text)), nil)
	c := a.Citations[0]
	if strings.Contains(c.Excerpt, "secret-token-123") {
		t.Fatalf("excerpt = %q, want the literal redacted", c.Excerpt)
	}
	if !strings.Contains(c.Excerpt, "WHERE token = '?'") ||
		!strings.Contains(c.Excerpt, "query 81 | SELECT * FROM api_keys") {
		t.Fatalf("excerpt = %q, want the redacted text", c.Excerpt)
	}
	if c.Digest != digestOf(text) {
		t.Fatalf("digest = %s, want the read's", c.Digest)
	}
}

func TestCompose_EmptyEvidenceTextHasNoExcerpt(t *testing.T) {
	a := compose("testdb", excerptResult(ev("config:all", "")), nil)
	c := a.Citations[0]
	if c.Excerpt != "" || c.ExcerptTruncated {
		t.Fatalf("citation = %+v, want no excerpt", c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "excerpt") {
		t.Fatalf("citation JSON = %s, want no excerpt keys", raw)
	}
}

func TestCompose_StoredAnswerWithoutExcerptsDecodes(t *testing.T) {
	// Nil/empty: an answer stored before excerpts existed.
	var c Citation
	stored := `{"id":"finding:17","kind":"finding","ref":"17","label":"x","digest":"ab"}`
	if err := json.Unmarshal([]byte(stored), &c); err != nil {
		t.Fatal(err)
	}
	if c.Excerpt != "" || c.ExcerptTruncated || c.ID != "finding:17" {
		t.Fatalf("decoded = %+v", c)
	}
}

func TestCompose_UnconcludedRunCarriesNoExcerpts(t *testing.T) {
	res := excerptResult(ev("finding:42", "text the model read"))
	res.Transcript.Stop = agentloop.StopWall
	a := compose("testdb", res, nil)
	if len(a.Citations) != 0 {
		t.Fatalf("citations = %+v, want none for a run that did not conclude", a.Citations)
	}
}
