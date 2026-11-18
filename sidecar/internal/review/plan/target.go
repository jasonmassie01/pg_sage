package plan

import (
	"fmt"
	"regexp"
	"strings"
)

// A hypothesis target is written as
//
//	[schema.]relation[(column, ...)] [key=value]
//	key=value
//
// for example public.orders(region, country), public.orders
// autovacuum_vacuum_scale_factor=0.05 or work_mem=64MB. Identifiers are
// plain (folded to lower case) or double-quoted without quotes inside;
// anything else is refused rather than interpreted. Hashed identifiers
// (review.hash_identifiers) are mapped back through the pack's pseudonyms.

// maxTargetRunes bounds a target.
const maxTargetRunes = 500

var (
	identBare   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)
	identQuoted = regexp.MustCompile(`^"[^"]+"$`)
	reParam     = regexp.MustCompile(`^([a-z_][a-z0-9_.]*)=([A-Za-z0-9_.+\-]+)$`)
)

// target is a parsed hypothesis target; names are canonical (unquoted ones
// folded to lower case).
type target struct {
	schema, relation string
	columns          []string
	hasColumns       bool
	key, value       string
}

// table is "schema.relation" as the pack writes it.
func (t target) table() string { return t.schema + "." + t.relation }

func parseTarget(s string, pseudonyms map[string]string) (target, error) {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > maxTargetRunes {
		return target{}, refuse(ReasonInvalidTarget, "the target is empty or longer than "+
			"%d characters", maxTargetRunes)
	}
	head, rest := s, ""
	if i := strings.IndexAny(s, " \t"); i >= 0 && !strings.Contains(s[:i], "(") {
		head, rest = s[:i], strings.TrimSpace(s[i:])
	} else if j := strings.LastIndex(s, ")"); j >= 0 {
		head, rest = s[:j+1], strings.TrimSpace(s[j+1:])
	}
	var t target
	if rest == "" && !strings.ContainsAny(head, "()\"") &&
		reParam.MatchString(strings.ToLower(head)) {
		head, rest = "", head
	}
	if rest != "" {
		m := reParam.FindStringSubmatch(strings.ToLower(rest))
		if m == nil {
			return target{}, refuse(ReasonInvalidTarget, "%q is not one key=value setting",
				rest)
		}
		t.key, t.value = m[1], rest[strings.Index(rest, "=")+1:]
	}
	if head == "" {
		return t, nil
	}
	if err := t.parseRelation(head, pseudonyms); err != nil {
		return target{}, err
	}
	return t, nil
}

func (t *target) parseRelation(head string, pseudonyms map[string]string) error {
	rel, cols, hasCols := head, "", false
	if i := strings.Index(head, "("); i >= 0 {
		if !strings.HasSuffix(head, ")") {
			return refuse(ReasonInvalidTarget, "%q does not close its column list", head)
		}
		rel, cols, hasCols = head[:i], head[i+1:len(head)-1], true
	}
	parts := strings.Split(rel, ".")
	if len(parts) > 2 {
		return refuse(ReasonInvalidTarget, "%q has more than schema and relation", rel)
	}
	if len(parts) == 1 {
		parts = []string{"public", parts[0]}
	}
	var err error
	if t.schema, err = ident(parts[0], pseudonyms); err != nil {
		return err
	}
	if t.relation, err = ident(parts[1], pseudonyms); err != nil {
		return err
	}
	t.hasColumns = hasCols
	if !hasCols {
		return nil
	}
	for _, c := range strings.Split(cols, ",") {
		name, err := ident(strings.TrimSpace(c), pseudonyms)
		if err != nil {
			return err
		}
		t.columns = append(t.columns, name)
	}
	return nil
}

// ident is one identifier's canonical name, mapped back when it is a
// pseudonym of the model tier.
func ident(raw string, pseudonyms map[string]string) (string, error) {
	switch {
	case identQuoted.MatchString(raw):
		raw = raw[1 : len(raw)-1]
		if name, ok := pseudonyms[raw]; ok {
			return name, nil
		}
		return raw, nil
	case identBare.MatchString(raw):
		if name, ok := pseudonyms[raw]; ok {
			return name, nil
		}
		return strings.ToLower(raw), nil
	}
	return "", refuse(ReasonInvalidTarget, "%q is not an identifier (plain, or "+
		"double-quoted without quotes inside)", raw)
}

// distinct refuses a repeated column.
func distinct(cols []string) error {
	seen := map[string]bool{}
	for _, c := range cols {
		if seen[c] {
			return refuse(ReasonInvalidTarget, "column %s is listed twice", c)
		}
		seen[c] = true
	}
	return nil
}

func (t target) String() string {
	s := t.table()
	if t.hasColumns {
		s += "(" + strings.Join(t.columns, ", ") + ")"
	}
	if t.key != "" {
		s += fmt.Sprintf(" %s=%s", t.key, t.value)
	}
	return strings.TrimPrefix(s, ".")
}
