package sre

import (
	"strings"
	"testing"
)

// CHECK-30 building block: redaction removes DSNs, credentials, bearer
// tokens, SQL string literals and raw vectors from untrusted text
// (application names, probe errors, identifiers) and leaves ordinary
// evidence text alone.
func TestRedactText(t *testing.T) {
	cases := []struct{ in, mustNot, want string }{
		{"postgres://admin:hunter2@db.internal:5432/prod?sslmode=require", "hunter2", ""},
		{"app postgresql://u:p4ss@h/db ok", "p4ss", ""},
		{"failed: password=hunter2 host=db", "hunter2", ""},
		{`dsn "user=x password='s3cr3t word' dbname=y"`, "s3cr3t", ""},
		{"Authorization: Bearer abc.def.ghi", "abc.def", ""},
		{"api_key: sk-live-123456", "sk-live", ""},
		{"WHERE email = 'alice@example.com'", "alice@example.com", ""},
		{"embedding [0.12, -0.5, 1e-3, 0.4, 0.9, 0.1, 0.2, 0.3, 0.33]", "0.12", ""},
		{"root blocker pid 4242 is idle in transaction", "",
			"root blocker pid 4242 is idle in transaction"},
		{"slot \"cdc\" retains 1073741824 bytes of WAL", "",
			"slot \"cdc\" retains 1073741824 bytes of WAL"},
		{"[1, 2, 3]", "", "[1, 2, 3]"},
		{"Review pg_sage's action and roll it back if it's implicated", "",
			"Review pg_sage's action and roll it back if it's implicated"},
		{"x IN ('a', 'b') AND y LIKE 'z%'", "'a'", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		got := RedactText(c.in)
		if c.mustNot != "" && strings.Contains(got, c.mustNot) {
			t.Errorf("RedactText(%q) = %q still contains %q", c.in, got, c.mustNot)
		}
		if c.mustNot != "" && !strings.Contains(got, "[redacted") {
			t.Errorf("RedactText(%q) = %q lacks a redaction marker", c.in, got)
		}
		if c.want != "" || c.in == "" {
			if got != c.want {
				t.Errorf("RedactText(%q) = %q, want it unchanged", c.in, got)
			}
		}
	}
}

func TestRedactJSON_WalksNestedStringsOnly(t *testing.T) {
	in := `{"rows":[{"application_name":"postgres://a:hunter2@h/d","backends":12,` +
		`"ok":true,"nested":{"error":"password=hunter2"}}],"n":1.5}`
	out, err := redactJSON([]byte(in))
	if err != nil {
		t.Fatalf("redactJSON: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "hunter2") || !strings.Contains(s, `"backends":12`) ||
		!strings.Contains(s, `"ok":true`) || !strings.Contains(s, `"n":1.5`) {
		t.Fatalf("redacted = %s", s)
	}
	if _, err := redactJSON([]byte("{not json")); err == nil {
		t.Fatal("malformed JSON redacted without error")
	}
	big := `{"queryid":1234567890123456789}`
	if out, _ := redactJSON([]byte(big)); !strings.Contains(string(out),
		"1234567890123456789") {
		t.Fatalf("redaction rounded a 64-bit integer: %s", out)
	}
}
