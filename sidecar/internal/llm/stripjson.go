package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSONShape identifies whether the expected JSON is an object or
// an array. The caller must know the shape because ambiguous LLM
// output (containing both `{` and `[`) would otherwise be
// extracted inconsistently.
type JSONShape int

const (
	// JSONObject expects a top-level `{ ... }`.
	JSONObject JSONShape = iota
	// JSONArray expects a top-level `[ ... ]`.
	JSONArray
	// JSONAuto tries object first, then array. Use this only when
	// the prompt genuinely allows either.
	JSONAuto
)

// StripJSON extracts a JSON literal from an LLM response that may
// contain thinking tokens, markdown fences, or surrounding prose.
//
// It is deliberately simple: find the first opening delimiter and
// the last matching closing delimiter. For truncated output (e.g.
// thinking models that exhausted the token budget), callers should
// first run the string through RepairTruncatedJSON to close an
// unclosed array.
//
// Returns the trimmed input unchanged if no delimiters are found;
// callers should treat that as a parse failure at the json.Unmarshal
// step rather than silently succeeding.
func StripJSON(s string, shape JSONShape) string {
	s = stripFences(strings.TrimSpace(s))
	switch shape {
	case JSONObject:
		if out, ok := extract(s, '{', '}'); ok {
			return out
		}
	case JSONArray:
		if out, ok := extract(s, '[', ']'); ok {
			return out
		}
	case JSONAuto:
		if out, ok := extract(s, '{', '}'); ok {
			return out
		}
		if out, ok := extract(s, '[', ']'); ok {
			return out
		}
	}
	return s
}

// ParseJSON is the unified LLM response parser. It strips
// surrounding prose/fences, extracts the expected JSON shape,
// and unmarshals into out. On the first unmarshal failure it
// retries with RepairTruncatedJSON, which salvages truncated
// array responses from thinking models that exhaust the token
// budget. The returned error reports both attempts so callers
// can log the raw failure cause.
//
// Empty or "[]"/"{}" responses unmarshal into the zero value of
// out and return nil — callers should inspect out for emptiness
// rather than relying on an error.
func ParseJSON(raw string, shape JSONShape, out any) error {
	cleaned := strings.TrimSpace(StripJSON(raw, shape))
	if cleaned == "" {
		return nil
	}
	// Short-circuit when the response is the empty container for the
	// requested shape. Leave out at its zero value (e.g. nil slice)
	// so callers that treat "nothing recommended" as nil work
	// unchanged. A shape mismatch ("{}" when an array was expected)
	// is not short-circuited — it will fail Unmarshal below, which
	// is the correct behavior.
	switch {
	case shape == JSONArray && cleaned == "[]":
		return nil
	case shape == JSONObject && cleaned == "{}":
		return nil
	case shape == JSONAuto && (cleaned == "[]" || cleaned == "{}"):
		return nil
	}
	if err := json.Unmarshal([]byte(cleaned), out); err != nil {
		repaired := RepairTruncatedJSON(cleaned)
		if err2 := json.Unmarshal([]byte(repaired), out); err2 != nil {
			return fmt.Errorf(
				"json unmarshal: %w (repair also failed: %v, "+
					"response: %.200s)",
				err, err2, cleaned,
			)
		}
	}
	return nil
}

// extract returns s[first..last] inclusive if both delimiters are
// present and first < last; otherwise (s, false).
func extract(s string, open, close byte) (string, bool) {
	first := strings.IndexByte(s, open)
	if first < 0 {
		return s, false
	}
	last := strings.LastIndexByte(s, close)
	if last <= first {
		return s, false
	}
	return s[first : last+1], true
}

// stripFences removes surrounding ```json ... ``` markdown fences.
// It handles the variants produced by Gemini and OpenAI: the opening
// fence may be ``` or ```json (optionally followed by a newline),
// and the closing fence is always ```.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Strip opening fence up through the first newline (if any).
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		// Single-line fence — drop the leading ``` or ```json.
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}
