package pgconf

import "strings"

// DatabaseStmt is an ALTER DATABASE SET/RESET of one setting. Database keeps
// identifier quoting; an inverse must preserve its case and embedded quotes.
type DatabaseStmt struct {
	Database string
	SystemStmt
}

func ParseAlterDatabase(sql string) (DatabaseStmt, bool) {
	s := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sql), ";"))
	rest, ok := cutKeywords(s, "ALTER", "DATABASE")
	if !ok {
		return DatabaseStmt{}, false
	}
	database, clause := splitIdentifier(rest)
	if database == "" {
		return DatabaseStmt{}, false
	}
	stmt, ok := ParseAlterSystem("ALTER SYSTEM " + clause)
	return DatabaseStmt{Database: database, SystemStmt: stmt}, ok
}
