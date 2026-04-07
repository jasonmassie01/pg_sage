package sanitize

import "testing"

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "users", `"users"`},
		{"empty", "", `""`},
		{"with-quote", `bad"name`, `"bad""name"`},
		{"with-double-quote", `a""b`, `"a""""b"`},
		{"with-spaces", "my table", `"my table"`},
		{"with-semicolon", "drop;", `"drop;"`},
		{"unicode", "naïve", `"naïve"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QuoteIdentifier(tt.in)
			if got != tt.want {
				t.Errorf("QuoteIdentifier(%q) = %q, want %q",
					tt.in, got, tt.want)
			}
		})
	}
}

func TestQuoteQualifiedName(t *testing.T) {
	tests := []struct {
		schema string
		name   string
		want   string
	}{
		{"public", "users", `"public"."users"`},
		{"sage", "findings", `"sage"."findings"`},
		{`bad"sch`, `bad"tab`, `"bad""sch"."bad""tab"`},
		{"", "", `"".""`},
	}
	for _, tt := range tests {
		got := QuoteQualifiedName(tt.schema, tt.name)
		if got != tt.want {
			t.Errorf(
				"QuoteQualifiedName(%q,%q) = %q, want %q",
				tt.schema, tt.name, got, tt.want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"hello", "'hello'"},
		{"", "''"},
		{"o'brien", "'o''brien'"},
		{"a''b", "'a''''b'"},
		{"line\nbreak", "'line\nbreak'"},
	}
	for _, tt := range tests {
		got := QuoteLiteral(tt.in)
		if got != tt.want {
			t.Errorf("QuoteLiteral(%q) = %q, want %q",
				tt.in, got, tt.want)
		}
	}
}

func TestRejectMultiStatement(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{"single-no-semi", "SELECT 1", false},
		{"single-with-trailing-semi", "SELECT 1;", false},
		{
			"single-with-semi-and-whitespace",
			"SELECT 1;   \n\t",
			false,
		},
		{
			"multi-statement",
			"SELECT 1; SELECT 2",
			true,
		},
		{
			"sql-injection-attempt",
			"SELECT 1; DROP TABLE users",
			true,
		},
		{
			"semi-only",
			";",
			false,
		},
		{
			"empty",
			"",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RejectMultiStatement(tt.sql)
			if (err != nil) != tt.wantErr {
				t.Errorf(
					"RejectMultiStatement(%q): err=%v, wantErr=%v",
					tt.sql, err, tt.wantErr)
			}
		})
	}
}
