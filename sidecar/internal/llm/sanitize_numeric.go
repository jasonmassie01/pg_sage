package llm

import (
	"strings"
	"sync/atomic"
)

// minRedactedDigits is the shortest number RedactNumericLiterals hides:
// card numbers, SSNs and account ids are longer, LIMIT 10 is not.
const minRedactedDigits = 6

// keepNumbers is the inverse of llm.redact_numeric_literals so its zero
// value redacts: the switch fails closed before any config is applied.
var keepNumbers atomic.Bool

// SetRedactNumericLiterals turns numeric redaction in SanitizeForLLM and
// RedactLiteralsForLLM on or off for the whole process.
func SetRedactNumericLiterals(on bool) { keepNumbers.Store(!on) }

// RedactNumericLiteralsEnabled reports the process-wide switch.
func RedactNumericLiteralsEnabled() bool { return !keepNumbers.Load() }

// RedactLiteralsForLLM redacts string and dollar literals and, while the
// switch is on, long numbers in comparisons. Unlike SanitizeForLLM it keeps
// comments: it is for log messages, where "--" is ordinary text.
func RedactLiteralsForLLM(text string) string {
	out := RedactSQLLiterals(text)
	if RedactNumericLiteralsEnabled() {
		out = RedactNumericLiterals(out)
	}
	return out
}

// RedactNumericLiterals replaces every numeric literal of 6 or more digits
// that is compared with "?": after or before =, <>, !=, <, >, <= and >=,
// inside IN (...), ANY(ARRAY[...]) and the log form Key (c)=(...), and as
// a BETWEEN bound. A sign belongs to the number. Short numbers, numbers
// outside comparisons (LIMIT, OFFSET, type modifiers, function arguments),
// literals, quoted identifiers, dollar bodies and $n parameters are kept.
func RedactNumericLiterals(text string) string { return redactNumbers(text, true) }

// numScan is the scanner state: the previous significant token and a
// stack of open brackets, each marked when it holds a value list.
type numScan struct {
	text        string
	identifiers bool
	b           strings.Builder
	prev        string // "cmp", "cmp:ARRAY", "between", "word:<UPPER>", "open", ...
	lists       []bool
	between     bool
}

func redactNumbers(text string, identifiers bool) string {
	s := &numScan{text: text, identifiers: identifiers, prev: "other"}
	s.b.Grow(len(text))
	for i := 0; i < len(text); {
		i = s.step(i)
	}
	return s.b.String()
}

// step consumes one token at i and returns the index after it.
func (s *numScan) step(i int) int {
	text := s.text
	c := text[i]
	if end, ok := quotedTokenEnd(text, i, s.identifiers); ok {
		s.b.WriteString(text[i:end])
		s.prev = "other"
		return end
	}
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		s.b.WriteByte(c)
		return i + 1
	case isNumberStart(text, i):
		return s.number(i)
	case isTagStart(c) || c >= 0x80:
		return s.word(i)
	case c == '<' || c == '>' || c == '=' || c == '!':
		return s.operator(i)
	}
	s.punct(c)
	s.b.WriteByte(c)
	return i + 1
}

func (s *numScan) punct(c byte) {
	switch c {
	case '(', '[':
		s.lists = append(s.lists, s.opensList(c))
		s.prev = "open"
	case ')', ']':
		if len(s.lists) > 0 {
			s.lists = s.lists[:len(s.lists)-1]
		}
		s.prev = "other"
	case ',':
		s.prev = "comma"
	default:
		s.prev = "other"
	}
}

// opensList reports whether the bracket about to open holds compared
// values: IN (, ANY/ALL/SOME (, =( and the like, ARRAY[ or a nested
// bracket inside such a list.
func (s *numScan) opensList(c byte) bool {
	switch s.prev {
	case "cmp", "word:IN", "word:ANY", "word:ALL", "word:SOME":
		return c == '('
	case "word:ARRAY":
		return c == '[' && s.inList()
	case "cmp:ARRAY":
		return c == '['
	case "open", "comma":
		return s.inList()
	}
	return false
}

func (s *numScan) inList() bool { return len(s.lists) > 0 && s.lists[len(s.lists)-1] }

func (s *numScan) word(i int) int {
	j := i
	for j < len(s.text) && (isTagCont(s.text[j]) || s.text[j] == '$' || s.text[j] >= 0x80) {
		j++
	}
	w := strings.ToUpper(s.text[i:j])
	s.b.WriteString(s.text[i:j])
	switch {
	case w == "BETWEEN":
		s.between, s.prev = true, "between"
	case w == "SYMMETRIC" && s.prev == "between":
	case w == "AND" && s.between:
		s.between, s.prev = false, "between"
	case w == "ARRAY" && s.prev == "cmp":
		s.prev = "cmp:ARRAY" // x = ARRAY[...] compares every element
	default:
		s.prev = "word:" + w
	}
	return j
}

func (s *numScan) operator(i int) int {
	j := i
	for j < len(s.text) && strings.IndexByte("<>=!", s.text[j]) >= 0 {
		j++
	}
	op := s.text[i:j]
	s.b.WriteString(op)
	switch op {
	case "=", "<>", "!=", "<", ">", "<=", ">=":
		s.prev = "cmp"
	default:
		s.prev = "other"
	}
	return j
}

// number consumes the numeric literal at i and writes it, or "?" when it
// is long enough and compared.
func (s *numScan) number(i int) int {
	end, digits := numberEnd(s.text, i)
	if end < len(s.text) && (isTagCont(s.text[end]) || s.text[end] >= 0x80) {
		if !isDigit(s.text[i]) {
			// A sign or point before 5px or 5s: consume it alone so the
			// scan always advances, then read the rest as a word.
			s.punct(s.text[i])
			s.b.WriteByte(s.text[i])
			return i + 1
		}
		return s.word(i) // 123abc is not a number
	}
	compared := s.prev == "cmp" || s.prev == "between" ||
		((s.prev == "open" || s.prev == "comma") && s.inList()) ||
		comparisonFollows(s.text, end)
	if digits >= minRedactedDigits && compared {
		s.b.WriteByte('?')
	} else {
		s.b.WriteString(s.text[i:end])
	}
	s.prev = "other"
	return end
}

// isNumberStart reports a numeric literal at i: a digit, or a sign or
// point directly before a digit, not continuing an identifier or a $n
// parameter. A sign counts only where a value is expected.
func isNumberStart(text string, i int) bool {
	c := text[i]
	if precededByIdentifier(text, i) {
		return false
	}
	next := i+1 < len(text) && isDigit(text[i+1])
	switch {
	case isDigit(c):
		return true
	case c == '.':
		return next
	case c == '-' || c == '+':
		return next && signExpected(text, i)
	}
	return false
}

// signExpected reports whether a sign at i starts a value: the previous
// non-space byte is an operator, an opening bracket, a comma or nothing.
func signExpected(text string, i int) bool {
	j := i - 1
	for j >= 0 && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
		j--
	}
	return j < 0 || strings.IndexByte("=<>!([,", text[j]) >= 0
}

// numberEnd returns the end of the number at i and its digit count
// (mantissa digits; an exponent's digits do not count).
func numberEnd(text string, i int) (int, int) {
	j, digits := i, 0
	if text[j] == '-' || text[j] == '+' {
		j++
	}
	for j < len(text) && (isDigit(text[j]) || text[j] == '_' || text[j] == '.') {
		if isDigit(text[j]) {
			digits++
		}
		j++
	}
	if j+1 < len(text) && (text[j] == 'e' || text[j] == 'E') {
		k := j + 1
		if text[k] == '-' || text[k] == '+' {
			k++
		}
		if k < len(text) && isDigit(text[k]) {
			for k < len(text) && isDigit(text[k]) {
				k++
			}
			j = k
		}
	}
	return j, digits
}

// comparisonFollows reports a comparison operator after optional space
// at i, so the left operand of "123456789 = ssn" is redacted too.
func comparisonFollows(text string, i int) bool {
	for i < len(text) && strings.IndexByte(" \t\n\r", text[i]) >= 0 {
		i++
	}
	j := i
	for j < len(text) && strings.IndexByte("<>=!", text[j]) >= 0 {
		j++
	}
	switch text[i:j] {
	case "=", "<>", "!=", "<", ">", "<=", ">=":
		return true
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
