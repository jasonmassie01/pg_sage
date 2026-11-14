package collector

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/extschema"
)

// testPgss is pg_stat_statements' names in schema ext.
var testPgss = pgssNames{view: `"ext"."pg_stat_statements"`,
	info: `"ext"."pg_stat_statements_info"`, fn: `"ext"."pg_stat_statements"`}

// pgssStatements is every collector statement that reads
// pg_stat_statements, built over testPgss (security-01c-q: the names are
// qualified when the statement is built, not rewritten at run time).
func pgssStatements() map[string]string {
	counters := func(tpl string) string {
		return fmt.Sprintf(tpl, "blk_read_time", "blk_write_time", 1, 1, 1, testPgss.fn)
	}
	return map[string]string{
		"queryStatsSQL":                   counters(queryStatsSQL),
		"queryStatsWithWALSQL":            counters(queryStatsWithWALSQL),
		"queryStatsWithPlanTimeSQL":       counters(queryStatsWithPlanTimeSQL),
		"queryStatsWithWALAndPlanTimeSQL": counters(queryStatsWithWALAndPlanTimeSQL),
		"statementTextsSQL":               statementTextsSQL(testPgss.fn),
		"statementsEntriesSQL":            statementsEntriesSQL(testPgss.fn),
		"classifySQL":                     classifySQL(testPgss.view),
		"statementsDeallocSQL":            statementsDeallocSQL(testPgss.info),
		"statementsEpochSQL":              statementsEpochSQL(testPgss.info),
		"workloadRolesSQL":                workloadRolesSQL(testPgss.view),
	}
}

// bareRefs counts the pg_stat_statements references in sql not preceded by
// the "ext". qualifier; a setting name (pg_stat_statements.track_utility)
// is not a reference.
func bareRefs(sql string) (refs, bare int) {
	const name = "pg_stat_statements"
	for i := 0; ; {
		j := strings.Index(sql[i:], name)
		if j < 0 {
			return refs, bare
		}
		at := i + j
		if !strings.HasPrefix(sql[at+len(name):], ".") {
			refs++
			if !strings.HasSuffix(sql[:at], `"ext"."`) {
				bare++
			}
		}
		i = at + len(name)
	}
}

func TestPgssStatementsNameEveryRelationQualified(t *testing.T) {
	for name, sql := range pgssStatements() {
		refs, bare := bareRefs(sql)
		if refs == 0 || bare != 0 || strings.Contains(sql, "%!") {
			t.Errorf("%s: %d of %d references bare (want 0 of at least 1):\n%s",
				name, bare, refs, sql)
		}
	}
}

// The info-view probes take the qualified name as a parameter, never a
// bare to_regclass literal.
func TestInfoProbesTakeTheQualifiedName(t *testing.T) {
	for name, sql := range map[string]string{"settings": statementsSettingsSQL,
		"epoch probe": statementsInfoProbeSQL} {
		if !strings.Contains(sql, "to_regclass($1)") || strings.Contains(sql, "to_regclass('") {
			t.Errorf("%s probe = %s, want to_regclass($1)", name, sql)
		}
	}
	if !strings.Contains(capabilityKeySQL, "pg_catalog.pg_extension") ||
		strings.Contains(capabilityKeySQL, "to_regclass") {
		t.Errorf("capability key probe must find the view by pg_extension:\n%s",
			capabilityKeySQL)
	}
}

// Invalid input: a schema name with quotes and a dot stays one quoted
// identifier.
func TestNewPgssNamesQuotesHostileSchemaNames(t *testing.T) {
	got, err := newPgssNames(`o'dd "x".y`)
	if err != nil {
		t.Fatalf("newPgssNames: %v", err)
	}
	want := pgssNames{view: `"o'dd ""x"".y"."pg_stat_statements"`,
		info: `"o'dd ""x"".y"."pg_stat_statements_info"`,
		fn:   `"o'dd ""x"".y"."pg_stat_statements"`}
	if got != want {
		t.Fatalf("newPgssNames = %+v, want %+v", got, want)
	}
}

// Nil/empty: no schema is the undefined_table error the bare names raised,
// so the collector's "pg_stat_statements unavailable" path is unchanged.
func TestNewPgssNamesEmptySchemaIsAbsent(t *testing.T) {
	got, err := newPgssNames("")
	var pgErr *pgconn.PgError
	if got != (pgssNames{}) || !errors.Is(err, extschema.ErrExtensionAbsent) ||
		!errors.As(err, &pgErr) || pgErr.Code != "42P01" {
		t.Fatalf("newPgssNames(\"\") = %+v, %v; want absent with 42P01", got, err)
	}
}

// State transitions: the startup schema is used until a probe succeeds;
// then the probe's schema wins, including "" when the probe found none.
func TestStatementsSchemaFollowsTheProbe(t *testing.T) {
	cfg := testConfig()
	cfg.StatementsSchema = "startup"
	c := New(nil, cfg, 170000, noopLog)
	if got := c.statementsSchema(); got != "startup" {
		t.Fatalf("before a probe = %q, want the startup schema", got)
	}
	c.mu.Lock()
	c.caps.attempted, c.caps.probed, c.caps.key.schema = true, true, "moved"
	c.mu.Unlock()
	if got := c.statementsSchema(); got != "moved" {
		t.Fatalf("after a probe = %q, want the probed schema", got)
	}
	c.mu.Lock()
	c.caps.key.schema = ""
	c.mu.Unlock()
	if _, err := c.pgss(); !errors.Is(err, extschema.ErrExtensionAbsent) {
		t.Fatalf("pgss after a probe found none: %v, want absent", err)
	}
}

// No concurrent access test beyond the collector's own: statementsSchema
// reads the capability state under c.mu like every other reader of it.
