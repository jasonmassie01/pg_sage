package facts

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Subject patterns. Identifiers fold like PostgreSQL's: an unquoted part
// is lower-cased, a quoted part is taken exactly ("" escapes a quote). '*'
// matches any run of characters, in quoted parts too; nothing else is a
// wildcard. Index and table subjects are schema-qualified; a schema
// subject is one identifier; a slot subject is a slot name.

const (
	maxSubjectLen = 300
	maxIdentLen   = 63 // NAMEDATALEN - 1
)

var slotPattern = regexp.MustCompile(`^[a-z0-9_*]{1,63}$`)

// protectedSchemas are names no fact subject may match: pg_sage's own
// schema and the system schemas.
var protectedSchemas = []string{"sage", "pg_catalog", "information_schema", "pg_toast",
	"pg_temp_1", "pg_toast_temp_1"}

// Pattern is a validated subject pattern. Schema and Name hold the folded
// identifiers with '*' wildcards; Schema is empty for slots, Name for
// schemas.
type Pattern struct {
	Kind   Kind
	Schema string
	Name   string
}

// ObjectRef is an object a request or finding touches. An index may carry
// its table; KindRelation is a table or an index, unknown which.
type ObjectRef struct {
	Kind        Kind
	Schema      string
	Name        string
	TableSchema string
	TableName   string
}

type identPart struct {
	text   string
	quoted bool
}

// ParsePattern validates and folds a subject pattern of kind.
func ParsePattern(kind Kind, raw string) (Pattern, error) {
	s := strings.TrimSpace(raw)
	if len(s) == 0 || len(s) > maxSubjectLen {
		return Pattern{}, fmt.Errorf("%w: %q must be 1-%d characters", ErrInvalidSubject,
			raw, maxSubjectLen)
	}
	switch kind {
	case KindSlot:
		if !slotPattern.MatchString(s) {
			return Pattern{}, fmt.Errorf("%w: slot %q must be lower-case letters, digits, "+
				"_ or *", ErrInvalidSubject, raw)
		}
		return Pattern{Kind: KindSlot, Name: s}, nil
	case KindSchema, KindIndex, KindTable:
	default:
		return Pattern{}, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}
	parts, err := splitIdent(s)
	if err != nil {
		return Pattern{}, err
	}
	p, err := patternFromParts(kind, parts, raw)
	if err != nil {
		return Pattern{}, err
	}
	for _, protected := range protectedSchemas {
		if globMatch(p.Schema, protected) {
			return Pattern{}, fmt.Errorf("%w: %q could match schema %s", ErrProtectedSubject,
				raw, protected)
		}
	}
	return p, nil
}

func patternFromParts(kind Kind, parts []identPart, raw string) (Pattern, error) {
	if kind == KindSchema {
		if len(parts) != 1 {
			return Pattern{}, fmt.Errorf("%w: schema %q must be one identifier",
				ErrInvalidSubject, raw)
		}
		return Pattern{Kind: kind, Schema: parts[0].text}, nil
	}
	if len(parts) != 2 {
		return Pattern{}, fmt.Errorf("%w: %s %q must be schema-qualified (schema.name)",
			ErrInvalidSubject, kind, raw)
	}
	return Pattern{Kind: kind, Schema: parts[0].text, Name: parts[1].text}, nil
}

// String is the canonical spelling: folded identifiers bare when they
// need no quotes.
func (p Pattern) String() string {
	switch p.Kind {
	case KindSlot:
		return p.Name
	case KindSchema:
		return quoteIdent(p.Schema)
	}
	return quoteIdent(p.Schema) + "." + quoteIdent(p.Name)
}

// Matches reports whether the pattern covers ref. An unqualified ref
// matches any schema, and a relation of unknown kind matches both table
// and index patterns: when unsure, a fact binds (it can only narrow).
func (p Pattern) Matches(ref ObjectRef) bool {
	switch p.Kind {
	case KindSlot:
		return ref.Kind == KindSlot && globMatch(p.Name, ref.Name)
	case KindSchema:
		if ref.Kind == KindSlot {
			return false
		}
		return p.schemaMatches(ref.Schema) ||
			(ref.TableName != "" && p.schemaMatches(ref.TableSchema))
	case KindTable:
		if (ref.Kind == KindTable || ref.Kind == KindRelation) &&
			p.relMatches(ref.Schema, ref.Name) {
			return true
		}
		return ref.TableName != "" && p.relMatches(ref.TableSchema, ref.TableName)
	case KindIndex:
		return (ref.Kind == KindIndex || ref.Kind == KindRelation) &&
			p.relMatches(ref.Schema, ref.Name)
	}
	return false
}

func (p Pattern) schemaMatches(schema string) bool {
	return schema == "" || globMatch(p.Schema, schema)
}

func (p Pattern) relMatches(schema, name string) bool {
	return p.schemaMatches(schema) && globMatch(p.Name, name)
}

// String is the reference's display form.
func (r ObjectRef) String() string {
	if r.Kind == KindSlot {
		return "slot:" + r.Name
	}
	if r.Schema == "" {
		return quoteIdent(r.Name)
	}
	return quoteIdent(r.Schema) + "." + quoteIdent(r.Name)
}

// ParseObjectRef reads a target object: "slot:<name>", or a relation
// "schema.name" (optionally followed by "|..." as optimizer identifiers
// are). Other forms ("pid:123") are errors.
func ParseObjectRef(raw string) (ObjectRef, error) {
	s := strings.TrimSpace(raw)
	if name, ok := strings.CutPrefix(s, "slot:"); ok {
		if name == "" {
			return ObjectRef{}, fmt.Errorf("%w: empty slot target", ErrInvalidSubject)
		}
		return ObjectRef{Kind: KindSlot, Name: name}, nil
	}
	if i := strings.IndexByte(s, '|'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return ObjectRef{}, fmt.Errorf("%w: empty target", ErrInvalidSubject)
	}
	parts, err := splitIdent(s)
	if err != nil {
		return ObjectRef{}, err
	}
	switch len(parts) {
	case 1:
		return ObjectRef{Kind: KindRelation, Name: parts[0].text}, nil
	case 2:
		return ObjectRef{Kind: KindRelation, Schema: parts[0].text, Name: parts[1].text}, nil
	}
	return ObjectRef{}, fmt.Errorf("%w: target %q", ErrInvalidSubject, raw)
}

// splitIdent splits a dotted identifier into its parts.
func splitIdent(s string) ([]identPart, error) {
	var parts []identPart
	i := 0
	for {
		i = skipSpaces(s, i)
		part, next, err := readIdent(s, i)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		i = skipSpaces(s, next)
		if i == len(s) {
			return parts, nil
		}
		if s[i] != '.' {
			return nil, fmt.Errorf("%w: unexpected %q in %q", ErrInvalidSubject, s[i], s)
		}
		i++
	}
}

func skipSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// readIdent reads one quoted or unquoted identifier starting at i.
func readIdent(s string, i int) (identPart, int, error) {
	if i < len(s) && s[i] == '"' {
		return readQuoted(s, i+1)
	}
	start := i
	for i < len(s) && isIdentRune(rune(s[i])) {
		i++
	}
	text := s[start:i]
	if text == "" || (text[0] >= '0' && text[0] <= '9') || text[0] == '$' {
		return identPart{}, i, fmt.Errorf("%w: bad identifier in %q", ErrInvalidSubject, s)
	}
	if len(text) > maxIdentLen {
		return identPart{}, i, fmt.Errorf("%w: identifier longer than %d bytes",
			ErrInvalidSubject, maxIdentLen)
	}
	return identPart{text: strings.ToLower(text)}, i, nil
}

func readQuoted(s string, i int) (identPart, int, error) {
	var b strings.Builder
	for i < len(s) {
		c := s[i]
		if c == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i += 2
				continue
			}
			text := b.String()
			if text == "" || len(text) > maxIdentLen {
				return identPart{}, i, fmt.Errorf("%w: quoted identifier must be 1-%d "+
					"bytes", ErrInvalidSubject, maxIdentLen)
			}
			return identPart{text: text, quoted: true}, i + 1, nil
		}
		if c < 0x20 || c == 0x7f {
			return identPart{}, i, fmt.Errorf("%w: control character in identifier",
				ErrInvalidSubject)
		}
		b.WriteByte(c)
		i++
	}
	return identPart{}, i, fmt.Errorf("%w: unterminated quoted identifier in %q",
		ErrInvalidSubject, s)
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '$' || r == '*' || r < 0x80 && (unicode.IsLetter(r) ||
		unicode.IsDigit(r)) || r >= 0x80
}

// quoteIdent spells a folded identifier: bare when PostgreSQL would read
// it back unchanged (lower case, letters, digits, _ or *), else quoted.
func quoteIdent(name string) string {
	bare := name != "" && (name[0] < '0' || name[0] > '9')
	for _, r := range name {
		if !isBareRune(r) {
			bare = false
			break
		}
	}
	if bare {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// isBareRune reports a character an unquoted identifier keeps as is.
func isBareRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '*'
}

// globMatch reports whether s matches pattern, where '*' matches any run
// of characters and every other character matches itself.
func globMatch(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			p, mark = star+1, mark+1
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// likePattern turns a glob into a LIKE pattern (default escape '\').
func likePattern(glob string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`)
	return r.Replace(glob)
}
