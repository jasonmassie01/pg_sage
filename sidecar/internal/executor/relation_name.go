package executor

import "strings"

// relationPart is one identifier of a relation name and whether it was
// double-quoted.
type relationPart struct {
	text   string
	quoted bool
}

// sameRelation reports whether named (a finding's or proposal's object,
// in catalog form like public.Orders or as SQL writes it) and written (a
// statement's target, as SQL writes it) are the same relation. Both
// default to schema public. A written unquoted name folds to lower case;
// a named unquoted name matches either as is (catalog form) or folded (SQL
// form). A named name equal to the written one's catalog schema.relation
// matches too, so a catalog name holding a space or a dot binds. Anything
// else that does not parse as schema.relation never matches.
func sameRelation(named, written string) bool {
	w, ok := splitRelationName(written)
	if !ok {
		return false
	}
	for i := range w {
		if !w[i].quoted {
			w[i].text = strings.ToLower(w[i].text)
		}
	}
	if strings.TrimSpace(named) == w[0].text+"."+w[1].text {
		return true
	}
	n, ok := splitRelationName(named)
	if !ok {
		return false
	}
	for i := range w {
		want := w[i].text
		got := n[i].text
		if got != want && (n[i].quoted || strings.ToLower(got) != want) {
			return false
		}
	}
	return true
}

// splitRelationName splits a [schema.]relation name into its two parts,
// public when the schema is omitted.
func splitRelationName(name string) ([]relationPart, bool) {
	var parts []relationPart
	rest := strings.TrimSpace(name)
	for {
		part, tail, ok := cutIdentifier(rest)
		if !ok {
			return nil, false
		}
		parts = append(parts, part)
		if tail == "" {
			break
		}
		if !strings.HasPrefix(tail, ".") {
			return nil, false
		}
		rest = tail[1:]
	}
	switch len(parts) {
	case 1:
		return []relationPart{{text: "public"}, parts[0]}, true
	case 2:
		return parts, true
	}
	return nil, false
}

// cutIdentifier reads one identifier from the start of s: a quoted one
// ("" escapes a quote) or an unquoted run up to the next dot.
func cutIdentifier(s string) (relationPart, string, bool) {
	if !strings.HasPrefix(s, `"`) {
		end := strings.IndexAny(s, `."`)
		if end < 0 {
			end = len(s)
		}
		text := s[:end]
		return relationPart{text: text}, s[end:], text != "" && !strings.ContainsAny(text, " ")
	}
	var text strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			text.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			text.WriteByte('"')
			i++
			continue
		}
		return relationPart{text: text.String(), quoted: true}, s[i+1:], text.Len() > 0
	}
	return relationPart{}, "", false
}
