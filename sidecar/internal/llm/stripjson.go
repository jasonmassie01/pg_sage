package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ErrEmptyResponse reports that the model returned no content. It is a
// failure (content filter, reasoning consumed the output budget), never
// a valid "nothing to recommend" answer (G3-B10).
var ErrEmptyResponse = errors.New("LLM returned an empty response")

// maxJSONCandidates bounds how many opening delimiters StripJSON tries.
const maxJSONCandidates = 64

// StripJSON extracts a JSON literal from an LLM response that may
// contain thinking tokens, markdown fences, or surrounding prose.
//
// It scans candidate opening delimiters left to right and returns the
// first one that decodes as a complete JSON value of the requested
// shape, so bracketed prose ("I looked at [the plan]") is skipped. For
// JSONArray, an object answer (json_object mode) is converted: an object
// whose only field is an array yields that array, any other object is
// wrapped as a one-element array. A candidate truncated mid-value is
// returned from its opening delimiter to the end so RepairTruncatedJSON
// can salvage the complete elements.
//
// Returns the trimmed input unchanged if no delimiters are found;
// callers should treat that as a parse failure at the json.Unmarshal
// step rather than silently succeeding.
func StripJSON(s string, shape JSONShape) string {
	s = stripFences(strings.TrimSpace(s))
	if out, ok := scanJSON(s, shape); ok {
		return out
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
// A blank response returns ErrEmptyResponse. An explicit empty
// container ("[]"/"{}") unmarshals into the zero value of out and
// returns nil — callers should inspect out for emptiness.
func ParseJSON(raw string, shape JSONShape, out any) error {
	cleaned := strings.TrimSpace(StripJSON(raw, shape))
	if cleaned == "" {
		return ErrEmptyResponse
	}
	// Short-circuit when the response is the empty container for the
	// requested shape. Leave out at its zero value (e.g. nil slice)
	// so callers that treat "nothing recommended" as nil work
	// unchanged.
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

// scanJSON tries each opening delimiter in order and returns the first
// complete value that satisfies shape. See StripJSON. For JSONArray a
// decoded object is only a fallback: scanning resumes after it so a
// later top-level array wins, and arrays nested inside the object are
// never mistaken for the payload.
func scanJSON(s string, shape JSONShape) (string, bool) {
	fallback := ""
	tried := 0
	for i := 0; i < len(s) && tried < maxJSONCandidates; i++ {
		c := s[i]
		if (c != '[' && c != '{') || (shape == JSONObject && c != '{') {
			continue
		}
		tried++
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if fallback != "" {
				break
			}
			// Truncated value: hand the tail to the repair step.
			return strings.TrimSpace(s[i:]), true
		}
		if err != nil {
			continue // bracketed prose, not JSON
		}
		if shape != JSONArray || raw[0] == '[' {
			return string(raw), true
		}
		if fallback == "" {
			fallback = objectAsArray(raw)
		}
		i += int(dec.InputOffset()) - 1
	}
	return fallback, fallback != ""
}

// objectAsArray returns the array held by a single-field object (the
// json_object-mode wrapper {"items":[...]}), "[]" for an empty object,
// or wraps the object as a one-element array.
func objectAsArray(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err == nil && len(fields) == 0 {
		return "[]" // json_object mode's "nothing to recommend"
	}
	if len(fields) == 1 {
		for _, v := range fields {
			v = json.RawMessage(strings.TrimSpace(string(v)))
			if len(v) > 0 && v[0] == '[' {
				return string(v)
			}
		}
	}
	return "[" + string(raw) + "]"
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
