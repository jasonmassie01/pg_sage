package selfmonitor

import "testing"

// collection-11: the collector applies the statement self-exclusion in Go,
// so IsStatementText must leave out exactly what StatementExclusionSQL
// does. statementTextCases is shared with the parity test on PostgreSQL;
// a nil text is SQL NULL (the empty string in Go). Non-ASCII letters next
// to "sage." are left out: PostgreSQL's [[:alnum:]] there follows the
// database's locale.
var statementTextCases = []struct {
	text *string
	self bool
}{
	{ptr("/* pg_sage */ SELECT 1"), true},
	{ptr("SELECT /* pg_sage */ count(*) FROM pg_stat_activity"), true},
	{ptr("/* pg_sage text */ SELECT queryid FROM t"), true},
	{ptr("PG_SAGE"), true},
	{ptr("select Pg_Sage_version()"), true},
	{ptr("SELECT 'pg-sage' AS name"), true},    // ILIKE: _ is any one character
	{ptr("SELECT 'pg\nsage' AS name"), true},   // ... including a newline
	{ptr("SELECT * FROM sage.findings"), true}, // the sage schema
	{ptr(`SELECT * FROM "sage".findings`), true},
	{ptr("SELECT * FROM SAGE . findings"), true},
	{ptr("SELECT (sage).x FROM t"), false}, // a composite column
	{ptr("sage.snapshots"), true},
	{ptr("\tsage\t.x"), true},
	{ptr("SELECT * FROM my_sage.x"), false}, // another schema
	{ptr("SELECT * FROM message.x"), false},
	{ptr("SELECT usage.total FROM usage"), false},
	{ptr("SELECT pgsage FROM t"), false}, // no character between
	{ptr("SELECT sage FROM t"), false},   // a column, not a schema
	{ptr("SELECT 1"), false},
	{ptr(""), false},
	{nil, false},
	{ptr("<insufficient privilege>"), false},
}

func ptr(s string) *string { return &s }

func TestIsStatementText_Cases(t *testing.T) {
	for _, tc := range statementTextCases {
		text := ""
		if tc.text != nil {
			text = *tc.text
		}
		if got := IsStatementText(text); got != tc.self {
			t.Errorf("IsStatementText(%q) = %v, want %v", text, got, tc.self)
		}
	}
}
