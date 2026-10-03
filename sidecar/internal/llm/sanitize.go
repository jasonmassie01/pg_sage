package llm

import "strings"

// StripSQLComments removes block comments (/* ... */, including
// nested) and line comments (-- ...) from SQL text. Comment markers
// inside string literals (including E-strings with backslash escapes),
// quoted identifiers and dollar-quoted bodies are text, not comments, and
// are preserved.
func StripSQLComments(text string) string { return stripComments(text, true) }

// stripComments is StripSQLComments; identifiers=false treats double
// quotes as plain bytes (text that only resembles JSON, see SanitizeForLLM).
func stripComments(text string, identifiers bool) string {
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		if end, ok := quotedTokenEnd(text, i, identifiers); ok {
			b.WriteString(text[i:end])
			i = end
			continue
		}
		// Block comment: skip (handle nesting).
		if i+1 < len(text) && text[i] == '/' && text[i+1] == '*' {
			i = skipBlockComment(text, i)
			b.WriteByte(' ') // replace comment with space
			continue
		}
		// Line comment: skip to end of line.
		if i+1 < len(text) && text[i] == '-' && text[i+1] == '-' {
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

// quotedTokenEnd returns the end of the literal, quoted identifier or
// dollar-quoted body starting at i, or false when none starts there. An
// unterminated quoted identifier is not one: a stray double quote must not
// shield the rest of the text from redaction.
func quotedTokenEnd(text string, i int, identifiers bool) (int, bool) {
	switch {
	case isEStringStart(text, i):
		return skipSingleQuoted(text, i+1, true), true
	case text[i] == '\'':
		return skipSingleQuoted(text, i, false), true
	case text[i] == '"' && identifiers:
		end := skipQuotedIdentifier(text, i)
		return end, end > i+1 && text[end-1] == '"'
	case text[i] == '"':
		return i, false
	case text[i] == '$' && !precededByIdentifier(text, i):
		return skipDollarQuoted(text, i)
	}
	return i, false
}

// RedactSQLLiterals replaces single-quoted string literals and
// dollar-quoted literals with placeholders to prevent PII or
// secrets from leaking to an external LLM. It preserves SQL
// structure (keywords, identifiers, numbers, operators) so
// prompt-based analysis still works.
//
// Replacements:
//   - 'any text'     -> '?'
//   - an empty literal -> '?' (still redacted)
//   - E'any\'text'   -> '?' (backslash escapes honoured)
//   - $tag$ ... $tag$-> $?$
//   - $$ ... $$      -> $?$
//
// Doubled single quotes inside string literals are handled
// correctly: the whole literal is replaced, quotes and all. Quoted
// identifiers are copied verbatim: an apostrophe inside "o'brien" does not
// open a literal. A typed literal keeps its keyword: DATE'x' -> DATE'?'.
func RedactSQLLiterals(text string) string { return redactLiterals(text, true) }

func redactLiterals(text string, identifiers bool) string {
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		end, ok := quotedTokenEnd(text, i, identifiers)
		switch {
		case !ok:
			b.WriteByte(text[i])
			i++
			continue
		case text[i] == '"':
			b.WriteString(text[i:end])
		case text[i] == '$':
			b.WriteString("$?$")
		default:
			b.WriteString("'?'")
		}
		i = end
	}
	return b.String()
}

// isEStringStart reports an E'...' (or e'...') escape-string literal at
// i: the E must not end a longer word (DATE'...' is a typed literal).
func isEStringStart(text string, i int) bool {
	return (text[i] == 'E' || text[i] == 'e') && i+1 < len(text) &&
		text[i+1] == '\'' && !precededByIdentifier(text, i)
}

// precededByIdentifier reports whether text[i-1] can be part of an
// identifier, so text[i] continues a word (a$b$ is one identifier).
func precededByIdentifier(text string, i int) bool {
	if i == 0 {
		return false
	}
	c := text[i-1]
	return isTagCont(c) || c == '$' || c >= 0x80
}

// skipSingleQuoted returns the index after the closing ' of a
// single-quoted literal starting at start (which must point at the
// opening '). Doubled-quote pairs are consumed as part of the literal; with
// backslashEscapes (E-strings) so is any backslash-escaped byte. If the
// literal is unterminated, returns len(text).
func skipSingleQuoted(text string, start int, backslashEscapes bool) int {
	i := start + 1
	for i < len(text) {
		switch {
		case backslashEscapes && text[i] == '\\':
			i += 2
		case text[i] != '\'':
			i++
		case i+1 < len(text) && text[i+1] == '\'':
			i += 2 // doubled quote
		default:
			return i + 1
		}
	}
	return len(text)
}

// skipQuotedIdentifier returns the index after the closing " of a quoted
// identifier starting at start; "" inside is an escaped quote.
func skipQuotedIdentifier(text string, start int) int {
	i := start + 1
	for i < len(text) {
		if text[i] != '"' {
			i++
			continue
		}
		if i+1 < len(text) && text[i+1] == '"' {
			i += 2
			continue
		}
		return i + 1
	}
	return len(text)
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
//
// Plan JSON is sanitized per string value: there double quotes delimit
// JSON strings that hold SQL, not quoted identifiers. Text that only looks
// like JSON (it fails to parse) is scanned with double quotes as plain
// bytes, so literals inside its string values are still redacted.
func SanitizeForLLM(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if out, ok := sanitizeJSONStrings(trimmed); ok {
			return out
		}
		return redactLiterals(stripComments(text, false), false)
	}
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
