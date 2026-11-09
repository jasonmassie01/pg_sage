package sre

import (
	"regexp"
	"strings"
	"time"
)

// Numeric grounding (sre-03). A claim's numbers are compared with the
// numbers of the evidence it cites as exact tokens: an integer as its
// normalized signed decimal string (any size, so two 64-bit queryids that
// differ by one never collide), a decimal at the precision the evidence
// states it (75.310 is 75.31; 75.3 is not), and an RFC 3339 timestamp as
// one whole instant, never as its year, hour or second. Formatting is
// not evidence: a probe result's head line (probe version, row count),
// its [i] row ordinals and its omitted-row notices ground nothing.

var (
	numberPattern    = regexp.MustCompile(`\d+(?:\.\d+)?`)
	thousandsSep     = regexp.MustCompile(`(\d),(\d{3})\b`)
	timestampPattern = regexp.MustCompile(
		`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	// probeHead is the head line probes.Result.Text writes.
	probeHead = regexp.MustCompile(`(?m)^[a-z][a-z0-9_]* v\d+ ` +
		`(?:ok|empty|error|no_privilege|unsupported)` +
		`(?: historical_only=\S+ complete=\S+ exact_multiplicity=\S+ event_window=\[[^)\n]*\))?` +
		`(?:, \d+ rows)?`)
	rowOrdinal   = regexp.MustCompile(`(?m)^\[\d+\] `)
	omittedNotes = regexp.MustCompile(`\(\d+ more rows not shown\)`)
)

// groundToken is one number or timestamp: token as written, key as compared.
type groundToken struct {
	token string
	key   string
}

// groundSet is the set of token keys a claim may quote.
type groundSet map[string]bool

func (g groundSet) add(toks []groundToken) {
	for _, t := range toks {
		g[t.key] = true
	}
}

// textTokens is the grounding of formatted evidence text: its tokens once
// the probe format's artifacts are removed.
func textTokens(s string) []groundToken {
	s = probeHead.ReplaceAllString(s, "")
	s = rowOrdinal.ReplaceAllString(s, "")
	return groundTokens(omittedNotes.ReplaceAllString(s, ""))
}

// valueTokens is the grounding of typed values: each value on its own.
func valueTokens(values []string) []groundToken {
	var out []groundToken
	for _, v := range values {
		out = append(out, groundTokens(v)...)
	}
	return out
}

// groundTokens extracts the timestamps and numbers of s ("1,200" is 1200). A
// minus sign is a sign only at the start of s or after whitespace, "(",
// "=", ":", "[" or ",", so "10-20" is a range while "delta=-5" and the
// JSON array "[-5,-7]" are negative.
func groundTokens(s string) []groundToken {
	var out []groundToken
	s = timestampPattern.ReplaceAllStringFunc(s, func(ts string) string {
		out = append(out, groundToken{token: ts, key: "t:" + normalizeTimestamp(ts)})
		return " "
	})
	for {
		next := thousandsSep.ReplaceAllString(s, "$1$2")
		if next == s {
			break
		}
		s = next
	}
	for _, loc := range numberPattern.FindAllStringIndex(s, -1) {
		tok := s[loc[0]:loc[1]]
		if signed(s, loc[0]) {
			tok = "-" + tok
		}
		out = append(out, groundToken{token: tok, key: "n:" + normalizeNumber(tok)})
	}
	return out
}

// signed reports whether the digits at i carry a minus sign.
func signed(s string, i int) bool {
	if i == 0 || s[i-1] != '-' {
		return false
	}
	if i == 1 {
		return true
	}
	return strings.ContainsRune(" \t\r\n(=:[,", rune(s[i-2]))
}

// normalizeNumber is the comparison form of a decimal token: no leading
// integer zeros, no trailing fraction zeros, and "0" never negative.
func normalizeNumber(tok string) string {
	neg := strings.HasPrefix(tok, "-")
	tok = strings.TrimPrefix(tok, "-")
	whole, frac, _ := strings.Cut(tok, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	frac = strings.TrimRight(frac, "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

// normalizeTimestamp is the comparison form of an RFC 3339 token: the
// instant in UTC, so an offset spelling of the same instant matches.
func normalizeTimestamp(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts // not a valid instant: it matches only itself
	}
	return t.UTC().Format(time.RFC3339Nano)
}
