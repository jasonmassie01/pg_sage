package llm

import "strings"

// StripSQLComments removes block comments (/* ... */, including
// nested) and line comments (-- ...) from SQL text. Comments
// inside single-quoted string literals are preserved.
func StripSQLComments(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		// Single-quoted string literal: copy verbatim.
		if text[i] == '\'' {
			b.WriteByte(text[i])
			i++
			for i < len(text) {
				b.WriteByte(text[i])
				if text[i] == '\'' {
					// Escaped quote '' inside string.
					if i+1 < len(text) && text[i+1] == '\'' {
						b.WriteByte(text[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}

		// Block comment: skip (handle nesting).
		if i+1 < len(text) &&
			text[i] == '/' && text[i+1] == '*' {
			i = skipBlockComment(text, i)
			b.WriteByte(' ') // replace comment with space
			continue
		}

		// Line comment: skip to end of line.
		if i+1 < len(text) &&
			text[i] == '-' && text[i+1] == '-' {
			i += 2
			for i < len(text) && text[i] != '\n' {
				i++
			}
			continue
		}

		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// RedactSQLLiterals replaces single-quoted string literals and
// dollar-quoted literals with placeholders to prevent PII or
// secrets from leaking to an external LLM. It preserves SQL
// structure (keywords, identifiers, numbers, operators) so
// prompt-based analysis still works.
//
// Replacements:
//   - 'any text'     -> '?'
//   - ''             -> '?' (empty literal still redacted)
//   - $tag$ ... $tag$-> $?$
//   - $$ ... $$      -> $?$
//
// Escaped single quotes ('') inside string literals are handled
// correctly: the whole literal is replaced, quotes and all.
// E-strings (E'...') are treated like standard strings.
func RedactSQLLiterals(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		// E'...' or e'...' prefix.
		if (text[i] == 'E' || text[i] == 'e') &&
			i+1 < len(text) && text[i+1] == '\'' {
			i = skipSingleQuoted(text, i+1)
			b.WriteString("'?'")
			continue
		}
		// Standard single-quoted string.
		if text[i] == '\'' {
			i = skipSingleQuoted(text, i)
			b.WriteString("'?'")
			continue
		}
		// Dollar-quoted string: $tag$ ... $tag$ or $$ ... $$.
		if text[i] == '$' {
			if end, ok := skipDollarQuoted(text, i); ok {
				i = end
				b.WriteString("$?$")
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// skipSingleQuoted returns the index after the closing ' of a
// single-quoted literal starting at start (which must point at
// the opening '). Escaped '' pairs are consumed as part of the
// literal. If the literal is unterminated, returns len(text).
func skipSingleQuoted(text string, start int) int {
	i := start + 1
	for i < len(text) {
		if text[i] == '\'' {
			if i+1 < len(text) && text[i+1] == '\'' {
				i += 2 // escaped ''
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

// skipDollarQuoted detects a PostgreSQL dollar-quoted literal
// starting at start (which must point at '$'). If the delimiter
// is well-formed and a matching close is found, returns the
// index after the closing delimiter and true. Otherwise (not a
// valid dollar-quote start, or no close found), returns start, false
// so the caller falls through and writes the '$' byte literally.
func skipDollarQuoted(text string, start int) (int, bool) {
	// Parse tag: $tag$ where tag is [A-Za-z_][A-Za-z0-9_]* or empty.
	i := start + 1
	tagStart := i
	if i < len(text) {
		c := text[i]
		if c == '$' {
			// $$ tag
		} else if isTagStart(c) {
			i++
			for i < len(text) && isTagCont(text[i]) {
				i++
			}
			if i >= len(text) || text[i] != '$' {
				return start, false
			}
		} else {
			return start, false
		}
	} else {
		return start, false
	}
	tag := text[tagStart:i]
	bodyStart := i + 1
	// Scan body for closing $tag$.
	closer := "$" + tag + "$"
	j := bodyStart
	for j < len(text) {
		if j+len(closer) <= len(text) && text[j:j+len(closer)] == closer {
			return j + len(closer), true
		}
		j++
	}
	return start, false
}

func isTagStart(c byte) bool {
	return (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') || c == '_'
}

func isTagCont(c byte) bool {
	return isTagStart(c) || (c >= '0' && c <= '9')
}

// SanitizeForLLM applies the full pre-LLM sanitization: strip
// comments (which may contain prompt-injection text) and redact
// string literals (which may contain PII or secrets).
func SanitizeForLLM(text string) string {
	return RedactSQLLiterals(StripSQLComments(text))
}

// skipBlockComment advances past a /* ... */ block comment,
// handling nesting. Returns the index after the closing */.
func skipBlockComment(text string, start int) int {
	depth := 0
	i := start
	for i < len(text) {
		if i+1 < len(text) &&
			text[i] == '/' && text[i+1] == '*' {
			depth++
			i += 2
			continue
		}
		if i+1 < len(text) &&
			text[i] == '*' && text[i+1] == '/' {
			depth--
			i += 2
			if depth == 0 {
				return i
			}
			continue
		}
		i++
	}
	return i // unterminated comment: skip to end
}
