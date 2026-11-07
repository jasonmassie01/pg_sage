package briefing

import "strings"

// codeRegion is one fenced block or inline code span of markdown text.
// text[start:bodyStart] is the opening delimiter, text[bodyEnd:end] the
// closing one (empty for a fence left open at the end of the text).
type codeRegion struct {
	start, end         int
	bodyStart, bodyEnd int
	fence              bool
	lang               string
}

type fenceOpen struct {
	char byte
	n    int
	lang string
}

// codeRegions returns the fenced blocks and inline code spans of text, in
// order. Fences follow CommonMark (three or more backticks or tildes,
// closed by a run at least as long) except that any indentation counts.
func codeRegions(text string) []codeRegion {
	var out []codeRegion
	prose := 0
	for pos := 0; pos < len(text); {
		lineEnd := nextLine(text, pos)
		f, ok := parseFenceOpen(text[pos:lineEnd])
		if !ok {
			pos = lineEnd
			continue
		}
		out = append(out, inlineSpans(text, prose, pos)...)
		r := closeFence(text, pos, lineEnd, f)
		out = append(out, r)
		pos, prose = r.end, r.end
	}
	return append(out, inlineSpans(text, prose, len(text))...)
}

// nextLine returns the offset after the newline ending the line at pos.
func nextLine(text string, pos int) int {
	if i := strings.IndexByte(text[pos:], '\n'); i >= 0 {
		return pos + i + 1
	}
	return len(text)
}

// fenceRun returns the fence character and run length opening line. Any
// indentation is allowed: a fence nested in a list item is indented past
// CommonMark's three spaces, and missing it would let its body be read as
// one inline span whose first word is the language tag.
func fenceRun(line string) (byte, int, string) {
	trimmed := strings.TrimLeft(strings.TrimRight(line, "\r\n"), " \t")
	if trimmed == "" || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0, ""
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	return trimmed[0], n, trimmed[n:]
}

func parseFenceOpen(line string) (fenceOpen, bool) {
	c, n, info := fenceRun(line)
	if n < 3 || (c == '`' && strings.Contains(info, "`")) {
		return fenceOpen{}, false
	}
	lang := ""
	if fields := strings.Fields(info); len(fields) > 0 {
		lang = fields[0]
	}
	return fenceOpen{char: c, n: n, lang: lang}, true
}

// closeFence finds the line closing the fence opened at start.
func closeFence(text string, start, bodyStart int, f fenceOpen) codeRegion {
	r := codeRegion{start: start, bodyStart: bodyStart, fence: true, lang: f.lang,
		bodyEnd: len(text), end: len(text)}
	for pos := bodyStart; pos < len(text); {
		lineEnd := nextLine(text, pos)
		line := text[pos:lineEnd]
		c, n, rest := fenceRun(line)
		if c == f.char && n >= f.n && strings.TrimSpace(rest) == "" {
			r.bodyEnd = pos
			r.end = pos + len(strings.TrimRight(line, "\r\n"))
			return r
		}
		pos = lineEnd
	}
	return r
}

// inlineSpans returns the backtick code spans of text[from:to]. A run of
// backticks with no closing run of the same length is literal text.
func inlineSpans(text string, from, to int) []codeRegion {
	var out []codeRegion
	for i := from; i < to; {
		if text[i] != '`' {
			i++
			continue
		}
		n := backtickRun(text, i, to)
		closeAt := findBacktickRun(text, i+n, to, n)
		if closeAt < 0 {
			i += n
			continue
		}
		out = append(out, codeRegion{start: i, bodyStart: i + n,
			bodyEnd: closeAt, end: closeAt + n})
		i = closeAt + n
	}
	return out
}

func backtickRun(text string, i, to int) int {
	n := 0
	for i+n < to && text[i+n] == '`' {
		n++
	}
	return n
}

func findBacktickRun(text string, from, to, n int) int {
	for j := from; j < to; {
		if text[j] != '`' {
			j++
			continue
		}
		m := backtickRun(text, j, to)
		if m == n {
			return j
		}
		j += m
	}
	return -1
}
