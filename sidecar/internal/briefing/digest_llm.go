package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/llm"
)

// The digest headline (ux-02b): one sentence a configured model may write
// from the aggregated counts only (no database, object or statement ever
// reaches it). A reply that is not one plain sentence of at most
// maxHeadlineRunes, or that names an identifier from the digest, is
// refused and the deterministic headline stays, with the reason logged.

// maxHeadlineRunes bounds a model headline.
const maxHeadlineRunes = 200

// headlineMaxTokens caps the model's answer; a sentence needs far less.
const headlineMaxTokens = 200

// headlineTimeout bounds the one headline call, retries included, so a
// slow model never holds the scheduled digest. A variable for tests.
var headlineTimeout = 20 * time.Second

// The system prompt must not say "JSON": that turns on json_object mode.
const headlineSystem = `You are pg_sage, a PostgreSQL DBA agent. Write one plain
sentence, at most 200 characters, that tells the database owner what happened
since the last digest, from the counts provided. Use only these counts. Name no
database, table, index or other object, and write no SQL, markdown or code.

` + llm.UntrustedDataRule

var (
	qualifiedNameRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_$]*(?:\.[A-Za-z_][A-Za-z0-9_$]*)+`)
	underscoredRE   = regexp.MustCompile(`[A-Za-z0-9$]*_[A-Za-z0-9_$]*`)
)

// digestHeadline lets the configured model word d's headline; without a
// usable model, or on any refused reply, d keeps its deterministic one.
func (w *Worker) digestHeadline(ctx context.Context, d *Digest) {
	if w.llm == nil || !w.llm.IsEnabled() {
		return
	}
	if w.llm.IsCircuitOpen() {
		d.HeadlineFallback = "the model is paused after recent failures"
		return
	}
	headline, reason, cause := w.modelHeadline(ctx, *d)
	if reason != "" {
		d.HeadlineFallback = reason
		w.logFn("WARN", "briefing: [%s] digest headline: %s (%v); using the "+
			"deterministic headline", w.database, reason, cause)
		return
	}
	d.Headline, d.HeadlineSource = headline, HeadlineModel
}

// modelHeadline asks the model for a headline and validates it; a non-empty
// reason says why it was refused (stored with the digest), and cause is the
// error behind it (logged only: a model error can name its endpoint).
func (w *Worker) modelHeadline(ctx context.Context, d Digest) (string, string, error) {
	counts, err := json.Marshal(d.Counts)
	if err != nil {
		return "", "the counts could not be encoded", err
	}
	hctx, cancel := context.WithTimeout(llm.WithPurpose(ctx, "digest_headline"),
		headlineTimeout)
	defer cancel()
	resp, err := w.llm.ChatResult(hctx, headlineSystem,
		llm.UntrustedData("digest_counts", string(counts)), headlineMaxTokens)
	switch {
	case errors.Is(err, llm.ErrEmptyResponse):
		return "", "the reply was empty", err
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || hctx.Err() != nil):
		return "", "the model call timed out", err
	case err != nil:
		return "", "the model call failed", err
	}
	headline, err := validateHeadline(resp.Content, d)
	if err != nil {
		return "", err.Error(), err
	}
	return headline, "", nil
}

// validateHeadline returns reply as a headline, or why it is refused.
func validateHeadline(reply string, d Digest) (string, error) {
	h := strings.TrimSpace(reply)
	switch {
	case h == "":
		return "", errors.New("the reply was empty")
	case strings.HasPrefix(h, "```") || strings.HasPrefix(h, "{") ||
		strings.HasPrefix(h, "["):
		return "", errors.New("the reply was not a plain sentence (code block or object)")
	case strings.ContainsAny(h, "\r\n"):
		return "", errors.New("the reply was not one line")
	}
	if len(h) >= 2 && h[0] == '"' && h[len(h)-1] == '"' {
		h = strings.TrimSpace(h[1 : len(h)-1])
	}
	if utf8.RuneCountInString(h) > maxHeadlineRunes {
		return "", errors.New("the reply was longer than 200 characters")
	}
	if strings.Contains(h, "`") {
		return "", errors.New("the reply was not a plain sentence (code)")
	}
	for _, name := range forbiddenTerms(d) {
		if containsWord(h, name) {
			return "", errors.New("the reply named an identifier from the data")
		}
	}
	return h, nil
}

// containsWord reports name in s as a whole word, ignoring case.
func containsWord(s, name string) bool {
	s, name = strings.ToLower(s), strings.ToLower(name)
	for i := 0; ; {
		j := strings.Index(s[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		if (start == 0 || !identByte(s[start-1])) && (end == len(s) || !identByte(s[end])) {
			return true
		}
		i = start + 1
	}
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z'
}

// forbiddenTerms are the identifiers d's headline must not repeat, sorted.
func forbiddenTerms(d Digest) []string {
	return append([]string(nil), d.names...)
}

// digestNames collects the database, every held-back target object and
// the identifier-shaped words of the change descriptions: qualified names
// with each of their parts, and underscored words. Words shorter than 3
// characters are left out: they would refuse ordinary sentences.
func digestNames(in DigestInput) []string {
	set := map[string]bool{}
	add := func(name string) {
		for _, n := range append([]string{name}, strings.Split(name, ".")...) {
			if n = strings.Trim(n, `"`); len(n) >= 3 {
				set[n] = true
			}
		}
	}
	if in.Database != "" {
		add(in.Database)
	}
	for _, e := range in.HeldBack {
		for _, obj := range e.TargetObjects {
			add(obj)
		}
	}
	for _, r := range in.Outcomes.Rows {
		for _, text := range []string{r.What, r.Why} {
			for _, m := range qualifiedNameRE.FindAllString(text, -1) {
				add(m)
			}
			for _, m := range underscoredRE.FindAllString(text, -1) {
				add(m)
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
