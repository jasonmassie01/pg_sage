package auditchain

// The canonical expressions, for the golden tests in package auditchain_test.
var (
	SealedSQLForTest     = func(s Spec, alias string) string { return s.sealedSQL(alias) }
	SealedHashSQLForTest = func(s Spec, alias string) string { return s.sealedHashSQL(alias) }
	StateSQLForTest      = func(s Spec, alias string) string { return s.stateSQL(alias) }
	// RowPassSQLForTest is the full pass's ordered row-link batch query.
	RowPassSQLForTest = func(s Spec) string { return (&verifier{s: s}).rowPassSQL() }
)
