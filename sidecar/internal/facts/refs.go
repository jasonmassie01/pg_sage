package facts

import "strings"

// A small SQL reader: enough to name the objects of the statements pg_sage
// proposes (index and table DDL, maintenance, deletes, slot functions).
// It never decides safety; it only finds what a fact could be about, and
// an object it cannot name is matched conservatively by the targets.

type tokKind int

const (
	tokWord tokKind = iota
	tokQuoted
	tokString
	tokPunct
)

type token struct {
	kind tokKind
	text string
}

// scanSQL splits sql into words, quoted identifiers, string literals and
// punctuation, skipping comments.
func scanSQL(sql string) []token {
	var out []token
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			i = skipTo(sql, i, "\n")
		case strings.HasPrefix(sql[i:], "/*"):
			i = skipTo(sql, i+2, "*/")
		case c == '\'' || c == '"':
			text, next := readDelimited(sql, i+1, c)
			kind := tokString
			if c == '"' {
				kind = tokQuoted
			}
			out = append(out, token{kind: kind, text: text})
			i = next
		case isIdentRune(rune(c)) && c != '*':
			start := i
			for i < len(sql) && isIdentRune(rune(sql[i])) && sql[i] != '*' {
				i++
			}
			out = append(out, token{kind: tokWord, text: sql[start:i]})
		default:
			out = append(out, token{kind: tokPunct, text: string(c)})
			i++
		}
	}
	return out
}

func skipTo(s string, i int, end string) int {
	if j := strings.Index(s[i:], end); j >= 0 {
		return i + j + len(end)
	}
	return len(s)
}

// readDelimited reads a quoted literal whose quote is doubled to escape.
func readDelimited(s string, i int, quote byte) (string, int) {
	var b strings.Builder
	for i < len(s) {
		if s[i] == quote {
			if i+1 < len(s) && s[i+1] == quote {
				b.WriteByte(quote)
				i += 2
				continue
			}
			return b.String(), i + 1
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), i
}

// stmt is a scanned statement with a cursor.
type stmt struct {
	toks []token
	i    int
}

func (s *stmt) word(at int) string {
	if at < len(s.toks) && s.toks[at].kind == tokWord {
		return strings.ToUpper(s.toks[at].text)
	}
	return ""
}

// skipWords advances past each word in turn that is next.
func (s *stmt) skipWords(words ...string) {
	for _, w := range words {
		if s.word(s.i) == w {
			s.i++
		}
	}
}

// skipParens advances past a parenthesized option list.
func (s *stmt) skipParens() {
	if s.i >= len(s.toks) || s.toks[s.i].text != "(" || s.toks[s.i].kind != tokPunct {
		return
	}
	depth := 0
	for ; s.i < len(s.toks); s.i++ {
		switch t := s.toks[s.i]; {
		case t.kind == tokPunct && t.text == "(":
			depth++
		case t.kind == tokPunct && t.text == ")":
			depth--
			if depth == 0 {
				s.i++
				return
			}
		}
	}
}

// name reads a possibly qualified identifier at the cursor.
func (s *stmt) name() (schema, name string, ok bool) {
	part := func() (string, bool) {
		if s.i >= len(s.toks) {
			return "", false
		}
		t := s.toks[s.i]
		switch t.kind {
		case tokWord:
			s.i++
			return strings.ToLower(t.text), true
		case tokQuoted:
			s.i++
			return t.text, true
		}
		return "", false
	}
	first, ok := part()
	if !ok {
		return "", "", false
	}
	if s.i < len(s.toks) && s.toks[s.i].kind == tokPunct && s.toks[s.i].text == "." {
		s.i++
		second, ok := part()
		return first, second, ok
	}
	return "", first, true
}

// verb is the statement's leading keywords, normalized ("CREATE INDEX",
// "DROP INDEX", "ALTER TABLE", "VACUUM", ...).
func verb(toks []token) string {
	s := &stmt{toks: toks}
	first := s.word(0)
	switch first {
	case "CREATE":
		if s.word(1) == "UNIQUE" {
			return "CREATE " + s.word(2)
		}
		return "CREATE " + s.word(1)
	case "DROP", "ALTER", "COMMENT":
		return first + " " + s.word(1)
	}
	return first
}

// ResolveRefs names the objects a request touches: those its SQL names,
// then its targets (deduplicated).
func ResolveRefs(req Request) []ObjectRef {
	return mergeRefs(sqlRefs(scanSQL(req.SQL)), targetRefs(req.Targets))
}

// targetRefs parses a request's targets; one that is no object reference
// ("pid:123", "") is skipped.
func targetRefs(targets []string) []ObjectRef {
	var out []ObjectRef
	for _, raw := range targets {
		if ref, err := ParseObjectRef(raw); err == nil {
			out = append(out, ref)
		}
	}
	return out
}

// mergeRefs is refs followed by each of more it does not already hold, in
// a new slice (refs is never written).
func mergeRefs(refs, more []ObjectRef) []ObjectRef {
	out := append([]ObjectRef(nil), refs...)
	for _, ref := range more {
		if !containsRef(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}

func containsRef(refs []ObjectRef, ref ObjectRef) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

func sqlRefs(toks []token) []ObjectRef {
	s := &stmt{toks: toks}
	switch verb(toks) {
	case "CREATE INDEX":
		return createIndexRefs(s)
	case "CREATE STATISTICS":
		return fromRefs(s)
	case "DROP INDEX", "ALTER INDEX":
		s.i = 2
		s.skipWords("CONCURRENTLY", "IF", "EXISTS")
		return listRefs(s, KindIndex)
	case "ALTER TABLE", "DROP TABLE":
		s.i = 2
		s.skipWords("CONCURRENTLY", "IF", "EXISTS", "ONLY")
		return listRefs(s, KindTable)
	case "REINDEX":
		return reindexRefs(s)
	case "VACUUM", "ANALYZE", "CLUSTER", "TRUNCATE":
		s.i = 1
		s.skipParens()
		s.skipWords("TABLE", "FULL", "FREEZE", "VERBOSE", "ANALYZE", "ONLY")
		return listRefs(s, KindTable)
	case "DELETE":
		s.i = 1
		s.skipWords("FROM", "ONLY")
		return listRefs(s, KindTable)
	case "ALTER SYSTEM":
		return alterSystemRefs(toks)
	}
	return slotRefs(toks)
}

// slotBoundGUC is the cluster-wide setting past which PostgreSQL
// invalidates any slot that retains more WAL.
const slotBoundGUC = "max_slot_wal_keep_size"

// alterSystemRefs reads ALTER SYSTEM SET or RESET of max_slot_wal_keep_size
// as touching every slot of the cluster, whichever slot the proposal
// names. Only an exact -1 (unbounded) is exempt: RESET, DEFAULT and every
// finite value can later invalidate a slot, and a provider's default may
// be finite. A slot function in the statement is still named.
func alterSystemRefs(toks []token) []ObjectRef {
	refs := slotRefs(toks)
	if !boundsSlotWAL(toks) {
		return refs
	}
	return append([]ObjectRef{{Kind: KindSlot, AllSlots: true}}, refs...)
}

func boundsSlotWAL(toks []token) bool {
	s := &stmt{toks: toks}
	switch s.word(2) {
	case "RESET":
		return s.word(3) == "ALL" || isSlotBoundGUC(toks, 3)
	case "SET":
		return isSlotBoundGUC(toks, 3) && !unboundedValue(toks[4:])
	}
	return false
}

// isSlotBoundGUC reports the setting's name at i, bare or quoted, in any
// case (PostgreSQL looks setting names up case-insensitively).
func isSlotBoundGUC(toks []token, i int) bool {
	return i < len(toks) && (toks[i].kind == tokWord || toks[i].kind == tokQuoted) &&
		strings.EqualFold(toks[i].text, slotBoundGUC)
}

// unboundedValue reports "= -1" or "TO '-1'" (one optional trailing
// semicolon, nothing after it).
func unboundedValue(rest []token) bool {
	if len(rest) == 0 || !isPunct(rest[0], "=") &&
		(rest[0].kind != tokWord || !strings.EqualFold(rest[0].text, "TO")) {
		return false
	}
	v := rest[1:]
	if len(v) > 0 && isPunct(v[len(v)-1], ";") {
		v = v[:len(v)-1]
	}
	switch len(v) {
	case 1:
		return v[0].kind == tokString && v[0].text == "-1"
	case 2:
		return isPunct(v[0], "-") && v[1].kind == tokWord && v[1].text == "1"
	}
	return false
}

func isPunct(t token, text string) bool { return t.kind == tokPunct && t.text == text }

func createIndexRefs(s *stmt) []ObjectRef {
	s.i = 2
	if s.word(1) == "UNIQUE" {
		s.i = 3
	}
	s.skipWords("CONCURRENTLY", "IF", "NOT", "EXISTS")
	var idxSchema, idxName string
	if s.word(s.i) != "ON" {
		idxSchema, idxName, _ = s.name()
	}
	if s.word(s.i) != "ON" {
		return nil
	}
	s.i++
	s.skipWords("ONLY")
	tblSchema, tblName, ok := s.name()
	if !ok {
		return nil
	}
	table := ObjectRef{Kind: KindTable, Schema: tblSchema, Name: tblName}
	if idxName == "" {
		return []ObjectRef{table}
	}
	if idxSchema == "" {
		idxSchema = tblSchema
	}
	return []ObjectRef{{Kind: KindIndex, Schema: idxSchema, Name: idxName,
		TableSchema: tblSchema, TableName: tblName}, table}
}

func fromRefs(s *stmt) []ObjectRef {
	for s.i = 0; s.i < len(s.toks); s.i++ {
		if s.word(s.i) == "FROM" {
			s.i++
			return listRefs(s, KindTable)
		}
	}
	return nil
}

func reindexRefs(s *stmt) []ObjectRef {
	s.i = 1
	s.skipParens()
	kind := KindTable
	switch s.word(s.i) {
	case "INDEX":
		kind = KindIndex
	case "TABLE":
	default:
		return nil
	}
	s.i++
	s.skipWords("CONCURRENTLY")
	return listRefs(s, kind)
}

// listRefs reads a comma-separated list of names of kind.
func listRefs(s *stmt, kind Kind) []ObjectRef {
	var out []ObjectRef
	for {
		schema, name, ok := s.name()
		if !ok {
			return out
		}
		out = append(out, ObjectRef{Kind: kind, Schema: schema, Name: name})
		if s.i >= len(s.toks) || s.toks[s.i].text != "," {
			return out
		}
		s.i++
	}
}

// slotFunctions name a slot in their first argument.
var slotFunctions = map[string]bool{"PG_DROP_REPLICATION_SLOT": true,
	"PG_REPLICATION_SLOT_ADVANCE": true}

func slotRefs(toks []token) []ObjectRef {
	var out []ObjectRef
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind != tokWord || !slotFunctions[strings.ToUpper(toks[i].text)] ||
			toks[i+1].text != "(" || toks[i+2].kind != tokString {
			continue
		}
		ref := ObjectRef{Kind: KindSlot, Name: toks[i+2].text}
		if !containsRef(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}
