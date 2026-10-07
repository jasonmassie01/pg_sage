package executor

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/verify"
)

// verificationClass names the Phase 1.3 action class of a statement: what
// its verification measures (verify.Class*). It is "" for statements
// outside the verified classes (backend signals, ...), whose success is
// never credited without a post-check of their own.
func verificationClass(sql string) string {
	upper := strings.ToUpper(normalizeSQLText(sql))
	has := func(prefix string) bool { return strings.HasPrefix(upper, prefix) }
	switch {
	case IsIndexReplaceSQL(sql):
		return verify.ClassIndexReplace
	case has("CREATE INDEX") || has("CREATE UNIQUE INDEX"):
		return verify.ClassIndexCreate
	case has("DROP INDEX"):
		return verify.ClassIndexDrop
	case has("REINDEX"):
		return verify.ClassReindex
	case has("CREATE STATISTICS"):
		return verify.ClassStatistics
	case has("ALTER SYSTEM "):
		return verify.ClassGUC
	case has("ALTER DATABASE ") && (strings.Contains(upper, " SET ") ||
		strings.Contains(upper, " RESET ")):
		return verify.ClassGUC
	case has("ALTER TABLE ") && (strings.Contains(upper, " SET (") ||
		strings.Contains(upper, " RESET (")):
		return verify.ClassReloption
	case has("VACUUM"):
		return verify.ClassVacuum
	case has("ANALYZE"):
		return verify.ClassAnalyze
	case has("INSERT INTO HINT_PLAN.HINTS") || has("DELETE FROM HINT_PLAN.HINTS"):
		return verify.ClassQueryHint
	case has("DELETE FROM "):
		return verify.ClassRetention
	}
	return ""
}
