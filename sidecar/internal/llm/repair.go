package llm

import (
	"encoding/json"
	"regexp"
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
// consume the max_tokens output budget: Gemini 2.5+/3, DeepSeek
// R1/reasoner and Qwen QwQ. OpenAI's are matched in isThinkingModel.
var thinkingModelMarkers = []string{
	"gemini-2.5", "gemini-3", "gemini-2.0-flash-thinking",
	"deepseek-r1", "deepseek-reasoner", "qwq", "reasoning", "thinking",
}

// IsThinkingModel reports a model whose internal reasoning tokens
// consume the max_tokens output budget (Gemini 2.5+/3, OpenAI o-series
// and gpt-5+, DeepSeek R1, QwQ). Callers budget its reasoning separately.
func IsThinkingModel(model string) bool { return isThinkingModel(model) }

// ThinkingModel reports whether the client's configured model is a
// thinking model. A nil client is not.
func (c *Client) ThinkingModel() bool {
	if c == nil {
		return false
	}
	cfg, _ := c.configSnapshot()
	return isThinkingModel(cfg.Model)
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
	if len(m) >= 2 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9' {
		return true
	}
	// OpenAI gpt-5 and later reason in plain chat too; their "-chat"
	// aliases are the non-reasoning models.
	return openAIReasoningFamily.MatchString(m) && !strings.Contains(m, "-chat")
}

// openAIReasoningFamily matches gpt-5, gpt-5.1, gpt-6-luna, gpt-12, ...
var openAIReasoningFamily = regexp.MustCompile(`^gpt-([5-9]|[1-9][0-9])([.-]|$)`)

// RepairTruncatedJSON salvages a truncated JSON object or array. It
// scans from the first '{' or '[', cuts after the last completed value
// and appends the closers of the containers still open there:
//
//	{"findings":[{"x":1},{"x":2   ->  {"findings":[{"x":1}]}
//	{"a":1,"b":[1,2               ->  {"a":1,"b":[1]}
//
// A scalar or string counts only once a ',' or a closer follows it, so a
// cut number or a key without its value is dropped. An object that is an
// array element is kept whole or not at all, so a salvaged list never
// holds a half-built element (a hint without its rationale). Text before
// the first delimiter is dropped. Input that is already valid JSON, has
// nothing salvageable, or would not repair to valid JSON is returned
// unchanged, byte for byte.
func RepairTruncatedJSON(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return s
	}
	cut, closers, ok := scanTruncated(s[start:])
	if !ok {
		return s
	}
	repaired := s[start:start+cut] + closers
	if !json.Valid([]byte(repaired)) {
		return s
	}
	return repaired
}

// truncScan is the state of one scanTruncated pass. stack holds the open
// containers ('{' or '['); elemObjects counts the open objects that are
// array elements, which may not be cut inside.
type truncScan struct {
	stack       []byte
	elemObjects int
	cut         int // end (exclusive) of the last completed value, or -1
	cutDepth    int // containers open at cut
}

// scanTruncated scans s, which starts with '{' or '[', and returns where
// to cut and the closers to append. ok is false when the root container
// closes (s is not truncated) or no value completed before the end.
func scanTruncated(s string) (cut int, closers string, ok bool) {
	st := truncScan{cut: -1}
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			escaped = c == '\\'
			inString = c != '"'
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			st.open(c)
		case c == '}' || c == ']':
			if st.close() {
				return 0, "", false
			}
			st.mark(i + 1)
		case c == ',':
			st.mark(i)
		}
	}
	if st.cut < 0 {
		return 0, "", false
	}
	return st.cut, st.closers(), true
}

func (st *truncScan) open(c byte) {
	if c == '{' && len(st.stack) > 0 && st.stack[len(st.stack)-1] == '[' {
		st.elemObjects++
		c = 'e' // an object that is an array element
	}
	st.stack = append(st.stack, c)
}

// close pops the innermost container and reports whether it was the root.
func (st *truncScan) close() bool {
	top := st.stack[len(st.stack)-1]
	st.stack = st.stack[:len(st.stack)-1]
	if top == 'e' {
		st.elemObjects--
	}
	return len(st.stack) == 0
}

// mark records end as a cut point unless it falls inside an array element
// object.
func (st *truncScan) mark(end int) {
	if st.elemObjects == 0 {
		st.cut, st.cutDepth = end, len(st.stack)
	}
}

// closers returns the closing delimiters for the containers open at cut,
// innermost first. Every container open at the cut is still open at the
// end of the scan: closing one would have recorded a later cut.
func (st *truncScan) closers() string {
	b := make([]byte, 0, st.cutDepth)
	for i := st.cutDepth - 1; i >= 0; i-- {
		if st.stack[i] == '[' {
			b = append(b, ']')
		} else {
			b = append(b, '}')
		}
	}
	return string(b)
}
