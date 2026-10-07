package executor

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/extstats"
)

// statementTarget names the relation an allow-listed statement changes, as
// the statement writes it ("" when it names none: settings, signals,
// database-wide REINDEX). The protected-schema check and the operator's
// typed-target lease both read it, so they always agree on the object.
func statementTarget(sql, prefix string) string {
	switch prefix {
	case "CREATE INDEX", "CREATE UNIQUE INDEX":
		return createIndexTable(sql)
	case "DROP INDEX":
		return firstObjectAfter(sql, "DROP INDEX", "CONCURRENTLY", "IF", "EXISTS")
	case "REINDEX":
		return reindexObject(sql)
	case "VACUUM":
		return vacuumObject(sql)
	case "ANALYZE":
		return firstObjectAfter(sql, "ANALYZE", "VERBOSE")
	case "ALTER TABLE":
		return firstObjectAfter(sql, "ALTER TABLE", "IF", "EXISTS", "ONLY")
	case "CREATE STATISTICS":
		if c, err := extstats.ParseCreate(sql); err == nil {
			return c.QualifiedTable()
		}
	case "DROP STATISTICS":
		if d, err := extstats.ParseDrop(sql); err == nil {
			return d.QualifiedName()
		}
	}
	return ""
}

// operatorLeaseTargets are the objects an operator's SQL changes: the
// typed-target lease an operator action takes covers exactly these.
func operatorLeaseTargets(sql string) []string {
	if _, targets, ok := replaceGateView(sql); ok {
		return targets
	}
	normalized := normalizeSQLText(strings.TrimSpace(sql))
	upper := strings.ToUpper(normalized)
	for _, prefix := range allowedPrefixes {
		if !strings.HasPrefix(upper, prefix) {
			continue
		}
		if target := statementTarget(normalized, prefix); target != "" {
			return []string{target}
		}
		return nil
	}
	return nil
}

// createIndexTable is the table after ON (skipping ONLY).
func createIndexTable(sql string) string {
	fields := strings.Fields(sql)
	for i := 0; i < len(fields)-1; i++ {
		if !strings.EqualFold(fields[i], "ON") {
			continue
		}
		next := i + 1
		if strings.EqualFold(fields[next], "ONLY") {
			next++
		}
		if next < len(fields) {
			return cleanupIdentifierToken(fields[next])
		}
		return ""
	}
	return ""
}

func firstObjectAfter(sql, prefix string, skip ...string) string {
	fields := strings.Fields(sql)
	prefixFields := strings.Fields(prefix)
	if len(fields) < len(prefixFields)+1 {
		return ""
	}
	i := len(prefixFields)
	for i < len(fields) && containsFold(skip, fields[i]) {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	return cleanupIdentifierToken(fields[i])
}

// reindexObject is the index or table a REINDEX names, past its option
// list, its scope keyword and CONCURRENTLY.
func reindexObject(sql string) string {
	fields := strings.Fields(sql)
	i := skipOptionList(fields, 1)
	if i >= len(fields) {
		return ""
	}
	switch strings.ToUpper(fields[i]) {
	case "DATABASE", "SYSTEM", "SCHEMA":
		return ""
	}
	i++
	if i < len(fields) && strings.EqualFold(fields[i], "CONCURRENTLY") {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	return cleanupIdentifierToken(fields[i])
}

// vacuumObject is the table a VACUUM names, past its parenthesized option
// list and its bare option keywords.
func vacuumObject(sql string) string {
	fields := strings.Fields(sql)
	for i := skipOptionList(fields, 1); i < len(fields); i++ {
		token := strings.Trim(fields[i], ",;")
		switch strings.ToUpper(token) {
		case "FULL", "FREEZE", "VERBOSE", "ANALYZE":
			continue
		}
		return cleanupIdentifierToken(token)
	}
	return ""
}

// skipOptionList returns the index past a parenthesized option list that
// starts at fields[i], or i when none starts there.
func skipOptionList(fields []string, i int) int {
	if i >= len(fields) || !strings.HasPrefix(fields[i], "(") {
		return i
	}
	for i < len(fields) && !strings.HasSuffix(strings.TrimRight(fields[i], ",;"), ")") {
		i++
	}
	return i + 1
}

func cleanupIdentifierToken(token string) string {
	token = strings.TrimSpace(token)
	token = strings.TrimRight(token, ";,")
	if idx := strings.Index(token, "("); idx > 0 {
		token = token[:idx]
	}
	return token
}
