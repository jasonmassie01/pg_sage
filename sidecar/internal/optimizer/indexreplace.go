package optimizer

import "strings"

// A replacement (roadmap 2.3) is one action of two statements: the wider
// index's CREATE INDEX CONCURRENTLY, then the DROP INDEX CONCURRENTLY of
// the index it subsumes. Its undo re-creates the old index from its
// definition and drops the new one. Both pairs travel as text through the
// finding, the approval queue and the card, so an approval binds to both
// statements; the executor splits and validates each statement and never
// runs a pair as one string (CONCURRENTLY cannot run in a transaction).

// IndexReplaceSQL is the statement pair replacing oldIndex (qualified) with
// the index createDDL builds.
func IndexReplaceSQL(createDDL, oldIndex string) string {
	return trimStatement(createDDL) + ";\nDROP INDEX CONCURRENTLY " +
		strings.TrimSpace(oldIndex) + ";"
}

// IndexReplaceRollbackSQL is a replacement's undo: re-create the old index
// (its pg_get_indexdef definition, built CONCURRENTLY) and drop the new one.
func IndexReplaceRollbackSQL(oldDefinition, newIndex string) string {
	return concurrentDefinition(trimStatement(oldDefinition)) +
		";\nDROP INDEX CONCURRENTLY IF EXISTS " + strings.TrimSpace(newIndex) + ";"
}

// SplitIndexReplaceSQL returns the two statements of a pair (without their
// semicolons): a CREATE [UNIQUE] INDEX CONCURRENTLY, then a DROP INDEX
// CONCURRENTLY. Anything else is not a pair.
func SplitIndexReplaceSQL(sql string) (create, drop string, ok bool) {
	parts := splitStatements(sql)
	if len(parts) != 2 {
		return "", "", false
	}
	create, drop = parts[0], parts[1]
	if !hasWords(create, "CREATE", "INDEX", "CONCURRENTLY") &&
		!hasWords(create, "CREATE", "UNIQUE", "INDEX", "CONCURRENTLY") {
		return "", "", false
	}
	if !hasWords(drop, "DROP", "INDEX", "CONCURRENTLY") {
		return "", "", false
	}
	return create, drop, true
}

func trimStatement(sql string) string {
	return strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
}

// concurrentDefinition is a CREATE [UNIQUE] INDEX definition in its
// CONCURRENTLY form (unchanged when it already is).
func concurrentDefinition(def string) string {
	if hasWords(def, "CREATE", "INDEX", "CONCURRENTLY") ||
		hasWords(def, "CREATE", "UNIQUE", "INDEX", "CONCURRENTLY") {
		return def
	}
	for _, prefix := range []string{"CREATE UNIQUE INDEX ", "CREATE INDEX "} {
		if len(def) >= len(prefix) && strings.EqualFold(def[:len(prefix)], prefix) {
			return def[:len(prefix)] + "CONCURRENTLY " + def[len(prefix):]
		}
	}
	return def
}

// hasWords reports whether sql starts with words (case-insensitive,
// separated by any whitespace).
func hasWords(sql string, words ...string) bool {
	fields := strings.Fields(sql)
	if len(fields) < len(words) {
		return false
	}
	for i, w := range words {
		if !strings.EqualFold(fields[i], w) {
			return false
		}
	}
	return true
}

// splitStatements splits sql at semicolons outside quoted literals and
// identifiers. Empty statements are kept, so ";;" is never a pair.
func splitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == ';':
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	if rest := strings.TrimSpace(cur.String()); rest != "" || len(out) == 0 {
		out = append(out, rest)
	}
	for _, s := range out {
		if s == "" {
			return nil
		}
	}
	return out
}
