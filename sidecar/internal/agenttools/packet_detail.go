package agenttools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Bounds of untrusted text copied into a packet.
const (
	maxUntrustedTitle = 1000
	maxUntrustedText  = 4000
	maxUntrustedQuery = 2000
	maxUntrustedCount = 10
)

var (
	wordPattern   = regexp.MustCompile(`^[a-z0-9_:.-]{1,100}$`)
	objectPattern = regexp.MustCompile(`^[A-Za-z0-9_.$"|(),:-]{1,300}$`)
	routePattern  = regexp.MustCompile(`^[a-z_]{1,32}$`)
	concurrently  = regexp.MustCompile(`(?im)\bCONCURRENTLY\b|^\s*VACUUM\b`)
	indexPattern  = regexp.MustCompile(`(?is)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+` +
		`(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?("(?:[^"]|"")+"|[A-Za-z_][\w$]*)\s+` +
		`ON\s+(?:ONLY\s+)?((?:"(?:[^"]|"")+"|[A-Za-z_][\w$]*)` +
		`(?:\.(?:"(?:[^"]|"")+"|[A-Za-z_][\w$]*))?)`)
)

// evidenceKeys are the finding detail values a packet cites, in order,
// with their units. Only numbers and booleans are copied.
var evidenceKeys = []struct{ key, unit string }{
	{"estimated_improvement_pct", "%"}, {"hypopg_validated", ""},
	{"mean_exec_time", "ms"}, {"total_exec_time", "ms"}, {"calls", "calls"},
	{"estimated_size_bytes", "bytes"}, {"confidence_score", ""}, {"bloat_pct", "%"},
	{"idx_scan", "scans"}, {"index_size_bytes", "bytes"},
}

// whatIfVerdicts are the what_if_verdict strings a packet may cite.
var whatIfVerdicts = map[string]bool{"verified": true, "unverified": true, "rejected": true}

const (
	migrationNote = "Add up as a new migration in the application's repository; " +
		"pg_sage does not run it. down is its rollback."
	factBoundNote = "A confirmed fact says the application's migrations own this " +
		"object, so pg_sage hands the change over instead of running it."
	nonTxNote = " Run it outside a transaction: CONCURRENTLY cannot run inside one."
)

// findingChange is the change a finding carries: a fact-bound source fix
// (detail.source_fix) first, else its recommended SQL.
func findingChange(f finding) (Change, error) {
	if fix, ok := f.Detail["source_fix"].(map[string]any); ok {
		if up := detailString(fix, "migration"); strings.TrimSpace(up) != "" {
			route := detailString(fix, "route")
			if !routePattern.MatchString(route) {
				route = "source_fix"
			}
			c := Change{Up: up, Down: detailString(fix, "down"), FactBound: true,
				Route: route, NonTransactional: concurrently.MatchString(up),
				Note: factBoundNote}
			return withTxNote(c), nil
		}
	}
	up := strings.TrimSpace(f.SQL)
	if up == "" {
		return Change{}, fmt.Errorf("%w: finding %d", ErrNoChange, f.ID)
	}
	c := Change{Up: up, Down: strings.TrimSpace(f.Rollback), Route: "migration",
		NonTransactional: concurrently.MatchString(up), Note: migrationNote}
	return withTxNote(c), nil
}

func withTxNote(c Change) Change {
	if c.NonTransactional {
		c.Note += nonTxNote
	}
	return c
}

// indexRef is the index a change creates; Schema is "" when unqualified.
type indexRef struct{ Schema, Name string }

func (r indexRef) String() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

// createdIndex is the named index the SQL creates, in its table's schema.
func createdIndex(sql string) *indexRef {
	m := indexPattern.FindStringSubmatch(sql)
	if m == nil {
		return nil
	}
	ref := &indexRef{Name: unquoteIdent(m[1])}
	if parts := splitQualified(m[2]); len(parts) == 2 {
		ref.Schema = unquoteIdent(parts[0])
	}
	return ref
}

// splitQualified splits schema.name at a dot outside double quotes.
func splitQualified(s string) []string {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQuote = !inQuote
		case s[i] == '.' && !inQuote:
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

// unquoteIdent folds an identifier as PostgreSQL does.
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

func targetObjects(f finding, index *indexRef) []string {
	out := []string{}
	if table := safeObject(detailString(f.Detail, "table")); table != "" {
		out = append(out, table)
	}
	if index != nil {
		out = append(out, safeObject(index.String()))
	}
	return out
}

// detailQueryIDs are the target query ids of a finding: detail.queryids,
// detail.queryid or detail.query_id (numbers or decimal strings), deduped.
func detailQueryIDs(detail map[string]any) []int64 {
	var raw []any
	if list, ok := detail["queryids"].([]any); ok {
		raw = append(raw, list...)
	}
	raw = append(raw, detail["queryid"], detail["query_id"])
	seen := map[int64]bool{}
	out := []int64{}
	for _, v := range raw {
		id, ok := detailInt(v)
		if !ok || id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func detailInt(v any) (int64, bool) {
	var text string
	switch n := v.(type) {
	case json.Number:
		text = n.String()
	case string:
		text = n
	default:
		return 0, false
	}
	id, err := ParseQueryID(text)
	return int64(id), err == nil
}

func detailNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func detailString(detail map[string]any, key string) string {
	s, _ := detail[key].(string)
	return s
}

// untrustedText copies the finding's free text, bounded.
func untrustedText(f finding) UntrustedText {
	u := UntrustedText{Title: clipBytes(f.Title, maxUntrustedTitle),
		Recommendation: clipBytes(f.Recommendation, maxUntrustedText), Queries: []string{}}
	for _, key := range []string{"llm_rationale", "rationale"} {
		if r := detailString(f.Detail, key); r != "" {
			u.Rationale = clipBytes(r, maxUntrustedText)
			break
		}
	}
	for _, key := range []string{"affected_queries", "queries"} {
		list, _ := f.Detail[key].([]any)
		for _, q := range list {
			if s, ok := q.(string); ok && len(u.Queries) < maxUntrustedCount {
				u.Queries = append(u.Queries, clipBytes(s, maxUntrustedQuery))
			}
		}
	}
	return u
}

// findingEvidence cites the finding's numbers (and its what-if verdict)
// with where each comes from.
func findingEvidence(f finding) []Evidence {
	out := []Evidence{}
	source := func(key string) string {
		return fmt.Sprintf("sage.findings#%d detail.%s", f.ID, key)
	}
	for _, k := range evidenceKeys {
		if n, ok := detailNumber(f.Detail[k.key]); ok {
			out = append(out, Evidence{Name: k.key, Value: n, Unit: k.unit, Source: source(k.key)})
		} else if b, ok := f.Detail[k.key].(bool); ok {
			out = append(out, Evidence{Name: k.key, Value: b, Source: source(k.key)})
		}
	}
	if v := detailString(f.Detail, "what_if_verdict"); whatIfVerdicts[v] {
		out = append(out, Evidence{Name: "what_if_verdict", Value: v,
			Source: source("what_if_verdict")})
	}
	return out
}
