package collector

import (
	"strings"
	"testing"
)

// statementsSQLs is every collector statement that reads pg_stat_statements.
// capabilityKeySQL also names the extension in a pg_extension literal, which
// unqualified would count: QuotesHostileSchemaNames checks its to_regclass.
func statementsSQLs() map[string]string {
	return map[string]string{
		"queryStatsSQL":                   queryStatsSQL,
		"queryStatsWithWALSQL":            queryStatsWithWALSQL,
		"queryStatsWithPlanTimeSQL":       queryStatsWithPlanTimeSQL,
		"queryStatsWithWALAndPlanTimeSQL": queryStatsWithWALAndPlanTimeSQL,
		"statementTextsSQL":               statementTextsSQL,
		"statementsEntriesSQL":            statementsEntriesSQL,
		"statementsClassifySQL":           statementsClassifySQL,
		"statementsSettingsSQL":           statementsSettingsSQL,
		"statementsDeallocSQL":            statementsDeallocSQL,
		"statementsInfoProbeSQL":          statementsInfoProbeSQL,
		"statementsEpochSQL":              statementsEpochSQL,
	}
}

// unqualified returns the references to the extension's relations in sql
// that are not qualified by prefix. A setting name
// (pg_stat_statements.track_utility) is not a relation.
func unqualified(sql, prefix string) (refs, bare int) {
	const name = "pg_stat_statements"
	for i := strings.Index(sql, name); i >= 0; {
		after := sql[i+len(name):]
		if !strings.HasPrefix(after, ".") {
			refs++
			if !strings.HasSuffix(sql[:i], prefix) {
				bare++
			}
		}
		next := strings.Index(after, name)
		if next < 0 {
			break
		}
		i += len(name) + next
	}
	return refs, bare
}

func TestQualifyStatements_QualifiesEveryRelation(t *testing.T) {
	for name, sql := range statementsSQLs() {
		before, _ := unqualified(sql, `"ext".`)
		if before == 0 {
			t.Errorf("%s: no pg_stat_statements reference to qualify", name)
			continue
		}
		got := qualifyStatements(sql, "ext")
		refs, bare := unqualified(got, `"ext".`)
		if bare != 0 || refs != before {
			t.Errorf("%s: %d of %d references unqualified (want 0 of %d):\n%s",
				name, bare, refs, before, got)
		}
		if strings.Contains(sql, "pg_stat_statements.track_utility") &&
			!strings.Contains(got, "current_setting('pg_stat_statements.track_utility'") {
			t.Errorf("%s: the setting name was rewritten:\n%s", name, got)
		}
	}
}

func TestQualifyStatements_EmptySchemaKeepsSQL(t *testing.T) {
	for name, sql := range statementsSQLs() {
		if got := qualifyStatements(sql, ""); got != sql {
			t.Errorf("%s: empty schema changed the SQL:\n%s", name, got)
		}
	}
}

// Invalid input: a schema name with quotes stays one identifier, and one
// literal inside to_regclass.
func TestQualifyStatements_QuotesHostileSchemaNames(t *testing.T) {
	schema := `o'dd "x"`
	got := qualifyStatements(capabilityKeySQL, schema)
	want := `to_regclass('"o''dd ""x""".pg_stat_statements')`
	if !strings.Contains(got, want) {
		t.Errorf("block time probe = %s\nwant it to contain %s", got, want)
	}
	got = qualifyStatements(statementsEntriesSQL, schema)
	if !strings.Contains(got, `FROM "o'dd ""x""".pg_stat_statements(false)`) {
		t.Errorf("entries SQL = %s", got)
	}
	got = qualifyStatements(statementsInfoProbeSQL, schema)
	if !strings.Contains(got, `to_regclass('"o''dd ""x""".pg_stat_statements_info')`) {
		t.Errorf("info probe = %s", got)
	}
}

func TestQualifyStatements_DoesNotDoubleQualify(t *testing.T) {
	once := qualifyStatements(queryStatsSQL, "ext")
	if strings.Contains(once, `"ext"."ext".`) || strings.Contains(once, `"ext".ext`) {
		t.Fatalf("double qualification:\n%s", once)
	}
	if n := strings.Count(once, `"ext".pg_stat_statements(`); n != 1 {
		t.Errorf("qualified function calls = %d, want 1 (the counters read)", n)
	}
}

// No concurrent access test: qualifyStatements is a pure function of its
// arguments and shares no state.
