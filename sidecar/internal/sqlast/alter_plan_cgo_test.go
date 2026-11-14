//go:build cgo

package sqlast

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// migration-02b: the online-migration planner reads its two ALTER TABLE
// forms from the parse tree. No concurrent-access test: parsing is a
// pure function of its input and keeps no state between calls.

func mustParseAlter(t *testing.T, sql string) PlannableAlter {
	t.Helper()
	got, err := ParsePlannableAlter(sql)
	if err != nil {
		t.Fatalf("ParsePlannableAlter(%q): %v", sql, err)
	}
	return got
}

func refuseAlter(t *testing.T, sql string, want error, fragments ...string) {
	t.Helper()
	got, err := ParsePlannableAlter(sql)
	if !errors.Is(err, want) {
		t.Fatalf("ParsePlannableAlter(%q) error = %v, want %v", sql, err, want)
	}
	if !reflect.DeepEqual(got, PlannableAlter{}) {
		t.Fatalf("ParsePlannableAlter(%q) refused but returned %#v", sql, got)
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("ParsePlannableAlter(%q) error = %q, want %q", sql, err, fragment)
		}
	}
}

func TestParsePlannableAlterReadsAddUnique(t *testing.T) {
	cases := []struct {
		sql  string
		want PlannableAlter
	}{
		{`ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`,
			PlannableAlter{Kind: AlterAddUnique, Schema: "public", Table: "users",
				Constraint: "users_email_key", Columns: []string{"email"}}},
		{`alter table Users add constraint "Users_Key" unique ("Email", Name);`,
			PlannableAlter{Kind: AlterAddUnique, Table: "users",
				Constraint: "Users_Key", Columns: []string{"Email", "name"}}},
		{`ALTER TABLE ONLY s . t ADD CONSTRAINT k UNIQUE ("we""ird")`,
			PlannableAlter{Kind: AlterAddUnique, Schema: "s", Table: "t",
				Constraint: "k", Columns: []string{`we"ird`}}},
		{`ALTER TABLE IF EXISTS public.users ADD CONSTRAINT k UNIQUE ("UNIQUE")`,
			PlannableAlter{Kind: AlterAddUnique, Schema: "public", Table: "users",
				Constraint: "k", Columns: []string{"UNIQUE"}}},
		// Comments are not clauses: the tree decides what the SQL says.
		{`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE /* ) INCLUDE ( */ (a)`,
			PlannableAlter{Kind: AlterAddUnique, Schema: "public", Table: "users",
				Constraint: "k", Columns: []string{"a"}}},
		{"ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (a) -- why\n",
			PlannableAlter{Kind: AlterAddUnique, Schema: "public", Table: "users",
				Constraint: "k", Columns: []string{"a"}}},
	}
	for _, tc := range cases {
		if got := mustParseAlter(t, tc.sql); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ParsePlannableAlter(%q)\n got %#v\nwant %#v", tc.sql, got, tc.want)
		}
	}
}

func TestParsePlannableAlterReadsSetNotNull(t *testing.T) {
	cases := map[string]PlannableAlter{
		`ALTER TABLE public.accounts ALTER COLUMN Email SET NOT NULL`: {
			Kind: AlterSetNotNull, Schema: "public", Table: "accounts", Column: "email"},
		`ALTER TABLE accounts ALTER "Email" SET NOT NULL;`: {
			Kind: AlterSetNotNull, Table: "accounts", Column: "Email"},
	}
	for sql, want := range cases {
		if got := mustParseAlter(t, sql); !reflect.DeepEqual(got, want) {
			t.Fatalf("ParsePlannableAlter(%q)\n got %#v\nwant %#v", sql, got, want)
		}
	}
}

func TestParsePlannableAlterCarriesTimingAsWritten(t *testing.T) {
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (email) `
	cases := []struct {
		options, timing          string
		deferrable, initDeferred bool
	}{
		{"", "", false, false},
		{"DEFERRABLE INITIALLY DEFERRED", " DEFERRABLE INITIALLY DEFERRED", true, true},
		{"initially deferred deferrable", " DEFERRABLE INITIALLY DEFERRED", true, true},
		{"INITIALLY DEFERRED", " INITIALLY DEFERRED", true, true},
		{"INITIALLY IMMEDIATE", " INITIALLY IMMEDIATE", false, false},
		{"NOT DEFERRABLE", " NOT DEFERRABLE", false, false},
		{"Deferrable Initially Immediate", " DEFERRABLE INITIALLY IMMEDIATE", true, false},
		{"NOT DEFERRABLE INITIALLY IMMEDIATE;", " NOT DEFERRABLE INITIALLY IMMEDIATE",
			false, false},
	}
	for _, tc := range cases {
		got := mustParseAlter(t, prefix+tc.options)
		if got.Timing != tc.timing || got.Deferrable != tc.deferrable ||
			got.InitDeferred != tc.initDeferred {
			t.Fatalf("%q: timing %q deferrable %v initdeferred %v, want %q %v %v",
				tc.options, got.Timing, got.Deferrable, got.InitDeferred,
				tc.timing, tc.deferrable, tc.initDeferred)
		}
	}
	// DEFERRED is an unreserved keyword: as a column it is not timing.
	got := mustParseAlter(t,
		`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (deferred) DEFERRABLE`)
	if !reflect.DeepEqual(got.Columns, []string{"deferred"}) || got.Timing != " DEFERRABLE" {
		t.Fatalf("column named deferred: %#v", got)
	}
}

func TestParsePlannableAlterRefusesOptionsItCannotCarry(t *testing.T) {
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE `
	cases := map[string]string{
		"(x) INCLUDE (y)":                 `"INCLUDE (y)"`,
		"(x) WITH (fillfactor=70)":        `"WITH (fillfactor=70)"`,
		"(x) USING INDEX TABLESPACE fast": `"USING INDEX TABLESPACE fast"`,
		"NULLS NOT DISTINCT (x)":          `"NULLS NOT DISTINCT (x)"`,
		"NULLS DISTINCT (x)":              `"NULLS DISTINCT (x)"`,
		"USING INDEX users_x_idx":         `"USING INDEX users_x_idx"`,
		"(x) DEFERRABLE DEFERRABLE":       `"DEFERRABLE"`,
	}
	for tail, echo := range cases {
		refuseAlter(t, prefix+tail, ErrUnsupportedOption, echo, "plan it by hand")
	}
	long := strings.Repeat("x", 200)
	_, err := ParsePlannableAlter(prefix + "(x) WITH (" + long + ")")
	if !errors.Is(err, ErrUnsupportedOption) || strings.Contains(err.Error(), long) {
		t.Fatalf("long option: error = %v, want a bounded unsupported option", err)
	}
}

func TestParsePlannableAlterRefusesOtherStatements(t *testing.T) {
	for _, sql := range []string{
		`ALTER TABLE t ALTER COLUMN a SET NOT NULL, ALTER COLUMN b SET NOT NULL`,
		`ALTER TABLE t ALTER COLUMN a SET NOT NULL; SELECT 1`,
		`ALTER TABLE t ADD PRIMARY KEY (a)`,
		`ALTER TABLE t ADD CONSTRAINT k PRIMARY KEY (a)`,
		`ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0)`,
		`ALTER TABLE t ADD UNIQUE (a)`,
		`ALTER TABLE t ALTER COLUMN a DROP NOT NULL`,
		`ALTER TABLE t ADD COLUMN x int`,
		`ALTER INDEX i SET (fillfactor = 70)`,
		`ALTER TABLE t RENAME COLUMN a TO b`,
		`ALTER TABLE db.public.t ALTER COLUMN a SET NOT NULL`,
		`ALTER TABLE t ADD CONSTRAINT Émail_key UNIQUE (a)`,
		`SELECT 1`,
	} {
		refuseAlter(t, sql, ErrNotPlannable, "unsupported migration statement")
	}
	refuseAlter(t, `ALTER TABLE t ADD CONSTRAINT k UNIQUE (a, A)`, ErrNotPlannable,
		`"a"`, "twice")
}

func TestParsePlannableAlterMapsParseErrors(t *testing.T) {
	const prefix = `ALTER TABLE public.users ADD CONSTRAINT k UNIQUE `
	cases := map[string]string{
		prefix + "()":          "plan it by hand",
		prefix + "(a) garbage": `at "garbage"`,
		// The grammar reports this one without a position.
		prefix + "(a) NOT VALID":                         "cannot be marked NOT VALID",
		prefix + "(a) NOT DEFERRABLE INITIALLY DEFERRED": "plan it by hand",
		prefix + "(a) DEFERRABLE INCLUDE (b)":            `at "INCLUDE (b)"`,
		prefix + `("a)`:                                  "plan it by hand",
		prefix + `("")`:                                  "plan it by hand",
		prefix + "(lower(a))":                            "plan it by hand",
		`ALTER TABLE public.users ALTER COLUMN a SET NOT NULL NOT VALID`: `at "NOT VALID"`,
	}
	for sql, fragment := range cases {
		refuseAlter(t, sql, ErrSyntax, "cannot parse", fragment)
	}
}

func TestParsePlannableAlterRefusesEmptyInput(t *testing.T) {
	for _, sql := range []string{"", "   ", ";", " ; ; ", "-- only a comment"} {
		refuseAlter(t, sql, ErrNotPlannable, "unsupported migration statement")
	}
}

// The scanner check, not the parser, refuses long names: the parser cuts
// them to 63 bytes with only a NOTICE, so a 64-byte name would otherwise
// reach the planner as a different, valid name.
func TestParsePlannableAlterRefusesNamesPostgresWouldTruncate(t *testing.T) {
	fits := strings.Repeat("c", maxNameBytes)
	got := mustParseAlter(t, `ALTER TABLE public.users ADD CONSTRAINT "`+fits+
		`" UNIQUE (email)`)
	if got.Constraint != fits {
		t.Fatalf("63-byte quoted name = %q", got.Constraint)
	}
	accents := strings.Repeat("é", 31) + "e"
	if got := mustParseAlter(t, `ALTER TABLE public.users ADD CONSTRAINT "`+accents+
		`" UNIQUE (email)`); got.Constraint != accents {
		t.Fatalf("63-byte multibyte name = %q", got.Constraint)
	}
	long := fits + "c"
	for _, sql := range []string{
		`ALTER TABLE public.users ADD CONSTRAINT "` + long + `" UNIQUE (email)`,
		`ALTER TABLE public.users ADD CONSTRAINT ` + long + ` UNIQUE (email)`,
		`ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (email, ` + long + `)`,
		`ALTER TABLE public.accounts ALTER COLUMN ` + long + ` SET NOT NULL`,
		`ALTER TABLE public.` + long + ` ALTER COLUMN email SET NOT NULL`,
		`ALTER TABLE public.users ADD CONSTRAINT "` + strings.Repeat("é", 32) +
			`" UNIQUE (email)`,
		`ALTER TABLE public.users ADD CONSTRAINT U&"` + long + `" UNIQUE (email)`,
	} {
		refuseAlter(t, sql, ErrNameTooLong, "63 bytes", "plan it by hand")
		if _, err := ParsePlannableAlter(sql); strings.Contains(err.Error(), long) {
			t.Fatalf("%q: error repeats the whole name: %v", sql, err)
		}
	}
}
