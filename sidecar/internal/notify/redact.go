package notify

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// urlPattern matches absolute URLs embedded in free text.
var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// RedactURL keeps only scheme and host: webhook secrets (Slack tokens,
// signed query strings) live in the path, query and userinfo.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted-url]"
	}
	if u.Path == "" && u.RawQuery == "" && u.User == nil {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + "://" + u.Host + "/[redacted]"
}

// RedactURLs replaces every URL inside s with its redacted form.
func RedactURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, RedactURL)
}

// RedactError strips URL secrets from an error. *url.Error embeds the full
// request URL (Go only masks userinfo passwords), so it is rebuilt with
// a redacted URL; other errors are redacted textually (G7-B10).
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		copied := *ue
		copied.URL = RedactURL(ue.URL)
		copied.Err = RedactError(ue.Err)
		return &copied
	}
	if RedactURLs(err.Error()) != err.Error() {
		return errors.New(RedactURLs(err.Error()))
	}
	return err
}

// TruncateRunes shortens s to at most max runes, ending with an ellipsis
// when cut, and never splits a UTF-8 sequence.
func TruncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeMrkdwn neutralizes Slack control sequences (<!channel>,
// <url|text>) in untrusted text such as query text or identifiers.
func EscapeMrkdwn(s string) string {
	return mrkdwnEscaper.Replace(s)
}

// sanitizeHeader removes CR/LF and other control characters that would
// let untrusted text inject additional mail headers (G7-B30).
func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || (r < 0x20 && r != '\t') || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
