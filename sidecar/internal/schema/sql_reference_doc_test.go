package schema

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// llm-10: docs/sql-reference.md is what operators query sage.briefings
// from, so its column table must name the columns the DDL creates, with
// their types.
//
// No concurrent-access tests: the parsers are pure functions over strings.

const sqlReferencePath = "../../../docs/sql-reference.md"

var (
	docColumnRowRE = regexp.MustCompile("^\\|\\s*(`[^|]+)\\|\\s*([a-z ]+?)\\s*\\|")
	docColumnRE    = regexp.MustCompile("`([a-z_][a-z0-9_]*)`")
	ddlColumnRE    = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s+([a-z]+)`)
)

// docTableColumns reads the column table under "### `sage.<table>`": the
// first cell names one or more backticked columns, the second their type.
func docTableColumns(doc, table string) (map[string]string, error) {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	heading := "### `sage." + table + "`"
	start := strings.Index(doc, heading+"\n")
	if start < 0 {
		return nil, fmt.Errorf("sql-reference.md: no %q section", heading)
	}
	cols := map[string]string{}
	for _, line := range strings.Split(doc[start:], "\n")[1:] {
		if strings.HasPrefix(line, "### ") || strings.HasPrefix(line, "---") {
			break
		}
		m := docColumnRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, name := range docColumnRE.FindAllStringSubmatch(m[1], -1) {
			if _, dup := cols[name[1]]; dup {
				return nil, fmt.Errorf("sql-reference.md: sage.%s.%s listed twice", table, name[1])
			}
			cols[name[1]] = m[2]
		}
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("sql-reference.md: %q has no column table", heading)
	}
	return cols, nil
}

// ddlTableColumns reads column names and base types from one CREATE TABLE
// statement. Constraint lines (PRIMARY KEY (...), UNIQUE, CHECK) are not
// columns.
func ddlTableColumns(ddl string) map[string]string {
	cols := map[string]string{}
	ddl = strings.ReplaceAll(ddl, "\r\n", "\n")
	open := strings.Index(ddl, "(")
	end := strings.LastIndex(ddl, ")")
	if open < 0 || end <= open {
		return cols
	}
	for _, line := range strings.Split(ddl[open+1:end], "\n") {
		m := ddlColumnRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch strings.ToLower(m[1]) {
		case "primary", "unique", "check", "constraint", "foreign", "exclude":
			continue
		}
		cols[m[1]] = m[2]
	}
	return cols
}

// columnDocProblems lists columns the doc names that the DDL lacks,
// columns the DDL has that the doc omits, and type mismatches.
// migrationAddColumnRE is one ADD COLUMN clause of an ALTER TABLE.
var migrationAddColumnRE = regexp.MustCompile(
	`(?i)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s+([a-z]+)`)

// migrationAddedColumns are the columns the migration steps add to
// sage.<table> with ALTER TABLE ... ADD COLUMN, by name and type: the
// table's documented shape is its CREATE TABLE plus these.
func migrationAddedColumns(statements []string, table string) map[string]string {
	cols := map[string]string{}
	alter := regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?sage\.` +
		regexp.QuoteMeta(table) + `\s+([^;]*)`)
	for _, sql := range statements {
		for _, m := range alter.FindAllStringSubmatch(sql, -1) {
			for _, c := range migrationAddColumnRE.FindAllStringSubmatch(m[1], -1) {
				cols[strings.ToLower(c[1])] = strings.ToLower(c[2])
			}
		}
	}
	return cols
}

func columnDocProblems(table string, doc, ddl map[string]string) []string {
	var problems []string
	for name, docType := range doc {
		ddlType, ok := ddl[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf(
				"sage.%s: doc names column %s, which the DDL does not create", table, name))
		case docType != ddlType:
			problems = append(problems, fmt.Sprintf(
				"sage.%s.%s: doc type %s, DDL type %s", table, name, docType, ddlType))
		}
	}
	for name := range ddl {
		if _, ok := doc[name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"sage.%s: DDL column %s is not documented", table, name))
		}
	}
	sort.Strings(problems)
	return problems
}

func readSQLReference(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(sqlReferencePath)
	if err != nil {
		t.Fatalf("read %s: %v", sqlReferencePath, err)
	}
	return string(raw)
}

func TestSQLReferenceBriefingsMatchesDDL(t *testing.T) {
	doc, err := docTableColumns(readSQLReference(t), "briefings")
	if err != nil {
		t.Fatal(err)
	}
	ddl := ddlTableColumns(ddlBriefings)
	if len(ddl) < 9 {
		t.Fatalf("parsed only %d DDL columns from ddlBriefings: %v", len(ddl), ddl)
	}
	added := migrationAddedColumns(migrationStatements(), "briefings")
	if added["digest_hash"] != "text" { // ux-02b's ddlBriefingsDigestHash
		t.Fatalf("migration steps add %v to sage.briefings, want digest_hash text", added)
	}
	for name, typ := range added {
		ddl[name] = typ
	}
	for _, p := range columnDocProblems("briefings", doc, ddl) {
		t.Error(p)
	}
}

func TestDDLTableColumnsParsesBriefings(t *testing.T) {
	got := ddlTableColumns(ddlBriefings)
	want := map[string]string{"id": "bigserial", "generated_at": "timestamptz",
		"period_start": "timestamptz", "period_end": "timestamptz", "mode": "text",
		"content_text": "text", "content_json": "jsonb", "llm_used": "boolean",
		"token_count": "integer", "delivery_status": "jsonb"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ddlTableColumns(ddlBriefings) = %v, want %v", got, want)
	}
}

const briefingsDocFixture = "### `sage.briefings`\n\nText.\n\n| Column | Type | Description |\n" +
	"|---|---|---|\n| `id` | bigserial | Primary key |\n" +
	"| `period_start`, `period_end` | timestamptz | Window |\n%s\n---\n\n" +
	"### `sage.next`\n\n| `outside` | text | not this table |\n"

func TestColumnDocProblemsFlagsMissingColumn(t *testing.T) {
	ddl := map[string]string{"id": "bigserial", "period_start": "timestamptz",
		"period_end": "timestamptz"}
	doc, err := docTableColumns(fmt.Sprintf(briefingsDocFixture,
		"| `content` | text | old name |"), "briefings")
	if err != nil {
		t.Fatal(err)
	}
	problems := columnDocProblems("briefings", doc, ddl)
	if len(problems) != 1 || !strings.Contains(problems[0], "column content") {
		t.Fatalf("want one problem naming column content, got %v", problems)
	}
}

func TestColumnDocProblemsFlagsUndocumentedAndType(t *testing.T) {
	ddl := map[string]string{"id": "bigserial", "period_start": "timestamptz",
		"period_end": "timestamptz", "delivery_status": "jsonb"}
	doc, err := docTableColumns(strings.Replace(fmt.Sprintf(briefingsDocFixture, ""),
		"| bigserial |", "| serial |", 1), "briefings")
	if err != nil {
		t.Fatal(err)
	}
	problems := columnDocProblems("briefings", doc, ddl)
	want := []string{
		"sage.briefings.id: doc type serial, DDL type bigserial",
		"sage.briefings: DDL column delivery_status is not documented",
	}
	if fmt.Sprint(problems) != fmt.Sprint(want) {
		t.Fatalf("problems = %v, want %v", problems, want)
	}
}

func TestColumnDocProblemsAcceptsMultiColumnRow(t *testing.T) {
	doc, err := docTableColumns(fmt.Sprintf(briefingsDocFixture, ""), "briefings")
	if err != nil {
		t.Fatal(err)
	}
	ddl := map[string]string{"id": "bigserial", "period_start": "timestamptz",
		"period_end": "timestamptz"}
	if p := columnDocProblems("briefings", doc, ddl); len(p) != 0 {
		t.Fatalf("matching table reported %v", p)
	}
	if _, ok := doc["outside"]; ok {
		t.Fatal("parser read past the section into sage.next")
	}
}

func TestDocTableColumnsEmptyAndMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"no section": "### `sage.other`\n\n| `id` | bigserial | x |\n",
		"no table":   "### `sage.briefings`\n\nProse only.\n\n---\n",
	}
	for name, doc := range cases {
		if cols, err := docTableColumns(doc, "briefings"); err == nil {
			t.Errorf("%s: want error, got %v", name, cols)
		}
	}
	dup := fmt.Sprintf(briefingsDocFixture, "| `id` | bigserial | again |")
	if _, err := docTableColumns(dup, "briefings"); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate column: want \"twice\" error, got %v", err)
	}
}

func TestDDLTableColumnsEmptyAndConstraints(t *testing.T) {
	if got := ddlTableColumns(""); len(got) != 0 {
		t.Errorf("empty DDL: %v", got)
	}
	got := ddlTableColumns("CREATE TABLE t (\n  a integer,\n  b text NOT NULL,\n" +
		"  PRIMARY KEY (a),\n  UNIQUE (b),\n  CONSTRAINT c CHECK (a > 0)\n);")
	if fmt.Sprint(got) != fmt.Sprint(map[string]string{"a": "integer", "b": "text"}) {
		t.Errorf("constraints parsed as columns: %v", got)
	}
}

// Integration: the bootstrapped table, ALTERs included, has exactly the
// documented columns. Skips (via requireDB) without SAGE_DATABASE_URL.
func TestSQLReferenceBriefingsMatchesLiveTable(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	doc, err := docTableColumns(readSQLReference(t), "briefings")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'briefings'`)
	if err != nil {
		t.Fatalf("list sage.briefings columns: %v", err)
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		live[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	for name := range doc {
		if !live[name] {
			t.Errorf("doc names sage.briefings.%s; the live table has no such column", name)
		}
	}
	for name := range live {
		if _, ok := doc[name]; !ok {
			t.Errorf("live column sage.briefings.%s is not documented", name)
		}
	}
	if len(live) < 10 {
		t.Errorf("live sage.briefings has %d columns, want at least 10", len(live))
	}
}

func TestMigrationAddedColumnsReadsOnlyTheNamedTable(t *testing.T) {
	stmts := []string{
		"SET LOCAL lock_timeout = '5s';\nALTER TABLE sage.briefings ADD COLUMN IF NOT EXISTS" +
			" digest_hash text;\nALTER TABLE sage.briefings_old ADD COLUMN x integer;",
		"ALTER TABLE IF EXISTS sage.briefings\n  ADD COLUMN a integer,\n" +
			"  ADD COLUMN IF NOT EXISTS b jsonb NOT NULL DEFAULT '{}';",
		"ALTER TABLE sage.findings ADD COLUMN IF NOT EXISTS c text;",
	}
	got := migrationAddedColumns(stmts, "briefings")
	want := map[string]string{"digest_hash": "text", "a": "integer", "b": "jsonb"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("migrationAddedColumns = %v, want %v", got, want)
	}
	if got := migrationAddedColumns(nil, "briefings"); len(got) != 0 {
		t.Fatalf("no statements: %v, want none", got)
	}
}
