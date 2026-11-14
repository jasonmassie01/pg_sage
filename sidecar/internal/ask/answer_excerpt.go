package ask

import (
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/llm"
)

// excerptOf is what a citation keeps of the text the model read: the
// text redacted again (the tools already redacted it; this is defence in
// depth) and clipped to MaxExcerptRunes runes. truncated reports a clip.
func excerptOf(text string) (excerpt string, truncated bool) {
	if text == "" {
		return "", false
	}
	red := llm.RedactLiteralsForLLM(text)
	excerpt = clip(red, MaxExcerptRunes)
	return excerpt, utf8.RuneCountInString(red) > MaxExcerptRunes
}

// capExcerpts keeps excerpts in citation order while they fit in
// MaxExcerptBytes. From the first that does not fit on, citations keep
// no text and are marked truncated, so the excerpts a reader sees are
// always the earliest ones.
func capExcerpts(cs []Citation) {
	left := MaxExcerptBytes
	for i := range cs {
		c := &cs[i]
		if c.Excerpt == "" {
			continue
		}
		if len(c.Excerpt) <= left {
			left -= len(c.Excerpt)
			continue
		}
		left = -1 // closed: no later citation keeps text
		c.Excerpt, c.ExcerptTruncated = "", true
	}
}
