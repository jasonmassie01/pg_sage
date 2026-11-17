package workpack

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// texts.json (evid-02c): the statement text of every queryid the workload
// section lists, as the window's newest queries snapshot held it. It is in
// the local tier only as written; the model tier sanitizes it
// (llm.SanitizeForLLMStrict) and the export carries it for a person. It
// has no evidence ids: a viewer resolving facts never reads a text.

const fileTexts = "texts.json"

// MaxTextBytes bounds one local-tier text; a longer one is cut at a
// character boundary and marked truncated.
const MaxTextBytes = 16 << 10

// Texts is the listed queryids' texts, in the workload section's order
// (families by exec time, then each family's queryids).
type Texts struct {
	Texts []QueryText `json:"texts"`
}

// QueryText is one queryid's statement text.
type QueryText struct {
	QueryID   string `json:"queryid"`
	Family    string `json:"family"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// buildTexts lists the texts of wl's queryids that have one.
func buildTexts(wl Workload, texts map[int64]string) Texts {
	out := Texts{Texts: []QueryText{}}
	for _, f := range wl.Families {
		for _, q := range f.QueryIDs {
			if !q.TextKnown {
				continue
			}
			qid, ok := parseQueryID(q.QueryID)
			text, known := texts[qid]
			if !ok || !known || strings.TrimSpace(text) == "" {
				continue
			}
			cut, truncated := cutBytes(text, MaxTextBytes)
			out.Texts = append(out.Texts, QueryText{QueryID: q.QueryID, Family: f.ID,
				Text: cut, Truncated: truncated})
		}
	}
	return out
}

// cutBytes is s cut to at most n bytes at a character boundary.
func cutBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

// cutChars is s cut to at most n characters.
func cutChars(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos], true
		}
		i++
	}
	return s, false
}

// parseQueryID reads a decimal queryid.
func parseQueryID(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}
