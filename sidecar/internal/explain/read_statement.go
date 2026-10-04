package explain

// ReadStatement returns query without a trailing terminator when it is a
// single read statement (SELECT, WITH, VALUES or TABLE), or an
// ErrExplainInvalidRequest error. The SRE investigator's plan-only
// EXPLAIN uses it on statement text it reads from pg_stat_statements.
func ReadStatement(query string) (string, error) { return explainBody(query) }
