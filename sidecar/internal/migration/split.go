package migration

// SplitStatements is sql's statements with comments stripped and every
// string literal replaced by '***', split on top-level semicolons: the
// text-mode statements of a script the SQL parser cannot read (Change
// Review in a build without libpg_query, or SQL the parser rejects).
func SplitStatements(sql string) []string {
	return splitStatements(sanitizeDDL(sql))
}
