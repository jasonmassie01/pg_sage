package llm

import (
	"encoding/json"
	"strings"
)

// UnwrapText returns plain prose from an LLM response. When json_mode is
// enabled, models wrap a requested plain-text answer in a JSON object
// (e.g. {"audit_note":"..."}); this extracts the inner string so audit
// notes and narratives are stored as prose, not JSON. Non-JSON input is
// returned trimmed and unchanged.
func UnwrapText(raw string) string {
	s := strings.TrimSpace(StripJSON(raw, JSONObject))
	if !strings.HasPrefix(s, "{") {
		return strings.TrimSpace(raw)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return strings.TrimSpace(raw)
	}
	for _, k := range []string{"audit_note", "note", "narrative",
		"explanation", "summary", "text", "answer"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	for _, v := range m {
		if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
			return strings.TrimSpace(str)
		}
	}
	return strings.TrimSpace(raw)
}

// thinkingModelMarkers identify models whose internal reasoning tokens
// consume the max_tokens output budget: Gemini 2.5+/3, OpenAI o-series,
// DeepSeek R1/reasoner and Qwen QwQ.
var thinkingModelMarkers = []string{
	"gemini-2.5", "gemini-3", "gemini-2.0-flash-thinking",
	"deepseek-r1", "deepseek-reasoner", "qwq", "reasoning", "thinking",
}

// isThinkingModel returns true for models whose internal reasoning
// tokens consume the max_tokens output budget.
func isThinkingModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, marker := range thinkingModelMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	// OpenAI o-series: o1, o3, o4 (optionally -mini/-pro, provider prefix).
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return len(m) >= 2 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9'
}

// RepairTruncatedJSON attempts to salvage a truncated JSON array by
// cutting after the last complete top-level element and closing the
// array. Nesting depth is tracked, so elements that themselves contain
// arrays or objects (e.g. "affected_queries":[...]) are handled.
//
// When thinking models exhaust the output token budget, the JSON
// response is cut mid-element:
//
//	[{"hint":"HashJoin(t1 t2)","q":["a"]},{"hint":"Set(work_mem
//
// becomes [{"hint":"HashJoin(t1 t2)","q":["a"]}]. Input that is not a
// truncated array is returned trimmed and unchanged.
func RepairTruncatedJSON(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "[")
	if start < 0 {
		return s
	}
	lastComplete, closed := lastCompleteElement(s, start)
	if closed || lastComplete < 0 {
		return s // complete array, or nothing salvageable
	}
	return s[start:lastComplete+1] + "]"
}

// lastCompleteElement scans the array opened at s[start] and returns
// the index of the final byte of the last complete top-level element,
// and whether the array itself was closed.
func lastCompleteElement(s string, start int) (int, bool) {
	lastComplete := -1
	depth := 1
	inString, escaped := false, false
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
			if depth == 0 {
				return lastComplete, true
			}
			if depth == 1 {
				lastComplete = i
			}
		}
	}
	return lastComplete, false
}
