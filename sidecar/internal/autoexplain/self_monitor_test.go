package autoexplain

import (
	"strings"
	"testing"
)

func TestCandidateSQLFiltersSelfMonitoringQueries(t *testing.T) {
	if !strings.Contains(candidateSQL(publicStatements), "current_database()") {
		t.Fatal("candidateSQL must scope pg_stat_statements to current database")
	}
	if !strings.Contains(candidateSQL(publicStatements), "NOT ILIKE '%pg_sage%'") {
		t.Fatal("candidateSQL must filter pg_sage application text")
	}
	if !strings.Contains(candidateSQL(publicStatements), `"?sage"?`) {
		t.Fatal("candidateSQL must filter sage schema references")
	}
}
