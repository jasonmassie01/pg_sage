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

// isThinkingModel returns true for models whose internal reasoning
// tokens consume the max_tokens output budget (Gemini 2.5+ series, which
// emit a thought_signature, and OpenAI o-series).
func isThinkingModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "gemini-2.5") ||
		strings.Contains(m, "gemini-3") ||
		strings.Contains(m, "gemini-2.0-flash-thinking") ||
		strings.Contains(m, "o1") ||
		strings.Contains(m, "o3")
}

// RepairTruncatedJSON attempts to salvage a truncated JSON array
// by finding the last complete object and closing the array.
//
// When thinking models exhaust the output token budget, the JSON
// response is cut mid-object:
//
//	[{"hint":"HashJoin(t1 t2)","rationale":"reason"},{"hint":"Set(work_mem
//
// This function finds the last complete `}` and appends `]`.
func RepairTruncatedJSON(s string) string {
	s = strings.TrimSpace(s)

	// Already looks complete — nothing to repair.
	start := strings.Index(s, "[")
	if start < 0 {
		return s
	}
	end := strings.LastIndex(s, "]")
	if end > start {
		return s // Has both [ and ] — let the caller parse as-is
	}

	// Find the last complete object (closing brace at depth 0 relative
	// to the object).
	lastComplete := -1
	depth := 0
	inString := false
	escaped := false
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				lastComplete = i
			}
		}
	}

	if lastComplete < 0 {
		return s // No complete object found
	}

	return s[start:lastComplete+1] + "]"
}
