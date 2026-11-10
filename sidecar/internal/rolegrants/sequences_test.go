package rolegrants

import "testing"

// No concurrent access tests: these are pure functions of their input.

func TestSequenceSelectSQL(t *testing.T) {
	cases := []struct {
		schemas []string
		want    string
	}{
		{[]string{"app"}, "GRANT SELECT ON ALL SEQUENCES IN SCHEMA app TO sage_agent"},
		{[]string{"a", `"My App"`},
			`GRANT SELECT ON ALL SEQUENCES IN SCHEMA a, "My App" TO sage_agent`},
		// Empty: new sequences go to public by default.
		{nil, "GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO sage_agent"},
	}
	for _, c := range cases {
		if got := SequenceSelectSQL(c.schemas, "sage_agent"); got != c.want {
			t.Errorf("SequenceSelectSQL(%q) = %q, want %q", c.schemas, got, c.want)
		}
	}
}

func TestSequenceSelectTargets(t *testing.T) {
	missing := SequenceSelect{Schemas: []string{"a", "b", "c"}, Missing: []string{"a", "b"}}
	if !missing.Lacking() {
		t.Fatal("missing schemas a and b: Lacking() = false")
	}
	if got, want := missing.GrantSQL("r"),
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA a, b TO r"; got != want {
		t.Fatalf("GrantSQL = %q, want %q", got, want)
	}
	if got, want := missing.What(), "SELECT on the sequences in schemas a, b"; got != want {
		t.Fatalf("What = %q, want %q", got, want)
	}
	all := SequenceSelect{Schemas: []string{"c"}}
	if all.Lacking() {
		t.Fatal("nothing missing: Lacking() = true")
	}
	if got, want := all.What(), "SELECT on the sequences in schema c"; got != want {
		t.Fatalf("What = %q, want %q", got, want)
	}
	var none SequenceSelect
	if none.Lacking() || none.GrantSQL("r") !=
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO r" {
		t.Fatalf("zero value: lacking=%v sql=%q", none.Lacking(), none.GrantSQL("r"))
	}
}

// The fix for a member that does not inherit pg_signal_backend depends on
// the version: WITH INHERIT exists from PostgreSQL 16; before it the role
// itself must inherit.
func TestSignalBackendGrantSQL(t *testing.T) {
	cases := []struct {
		sig  SignalBackend
		want string
	}{
		{SignalBackend{Version: 170000}, "GRANT pg_signal_backend TO r"},
		{SignalBackend{Version: 140000}, "GRANT pg_signal_backend TO r"},
		{SignalBackend{MemberOnly: true, Version: 160000},
			"GRANT pg_signal_backend TO r WITH INHERIT TRUE"},
		{SignalBackend{MemberOnly: true, Version: 159999}, "ALTER ROLE r INHERIT"},
		{SignalBackend{MemberOnly: true, Version: 140000}, "ALTER ROLE r INHERIT"},
		// Zero value: not checked; the plain grant.
		{SignalBackend{}, "GRANT pg_signal_backend TO r"},
	}
	for _, c := range cases {
		if got := c.sig.GrantSQL("r"); got != c.want {
			t.Errorf("%+v.GrantSQL = %q, want %q", c.sig, got, c.want)
		}
	}
}
