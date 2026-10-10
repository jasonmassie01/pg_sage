package pgaudit

import (
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

func TestParseSessionDDL(t *testing.T) {
	msg := `AUDIT: SESSION,3,1,DDL,CREATE INDEX,INDEX,public.orders_status,` +
		`"CREATE INDEX CONCURRENTLY orders_status ON orders (status, ""a,b"")",<not logged>`
	r, ok := Parse(msg)
	if !ok {
		t.Fatalf("not parsed: %q", msg)
	}
	want := Record{AuditType: "SESSION", StatementID: 3, SubstatementID: 1, Class: "DDL",
		Command: "CREATE INDEX", ObjectType: "INDEX", ObjectName: "public.orders_status",
		Statement: `CREATE INDEX CONCURRENTLY orders_status ON orders (status, "a,b")`}
	if r != want {
		t.Fatalf("parsed = %+v\nwant     %+v", r, want)
	}
}

func TestParseObjectReadWithoutParameterField(t *testing.T) {
	r, ok := Parse(`AUDIT: OBJECT,12,1,READ,SELECT,TABLE,public.accounts,` +
		`SELECT * FROM accounts`)
	if !ok || r.AuditType != "OBJECT" || r.Class != "READ" || r.Command != "SELECT" ||
		r.ObjectName != "public.accounts" || r.Statement != "SELECT * FROM accounts" {
		t.Fatalf("parsed = %+v ok=%v", r, ok)
	}
}

func TestParseRejectsOtherMessages(t *testing.T) {
	for _, msg := range []string{
		"",
		"duration: 12.3 ms",
		"AUDIT: ",
		"AUDIT: SESSION,1,1,DDL",
		"AUDIT: SESSION,x,1,DDL,CREATE TABLE,TABLE,t,CREATE TABLE t ()",
		"AUDIT: OTHER,1,1,DDL,CREATE TABLE,TABLE,t,CREATE TABLE t ()",
		`AUDIT: SESSION,1,1,DDL,CREATE TABLE,TABLE,t,"unterminated`,
	} {
		if r, ok := Parse(msg); ok {
			t.Fatalf("%q parsed as %+v", msg, r)
		}
	}
}

func entry(app, user, msg string) logwatch.LogEntry {
	return logwatch.LogEntry{Timestamp: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
		PID: 4242, SessionID: "abc.1", Database: "orders", User: user, ErrorLevel: "LOG",
		Message: msg, Application: app}
}

const ddl = `AUDIT: SESSION,1,1,DDL,CREATE TABLE,TABLE,public.t,CREATE TABLE t (a int)`

// Attribution follows application_name first ('pg_sage agent:<principal>
// [:<action>]', then pg_sage itself), then the agent role naming rule.
func TestAttribute(t *testing.T) {
	cases := []struct {
		app, user           string
		by, principal, role string
		action              int64
		ok                  bool
	}{
		{"pg_sage agent:ci-bot:42", "sage_agentb_k2m4q7x9ab", ByApplication, "ci-bot", "", 42,
			true},
		{"pg_sage agent:agp_aaaaaaaaaaaaaaaaaaaa", "x", ByApplication,
			"agp_aaaaaaaaaaaaaaaaaaaa", "", 0, true},
		{"pg_sage", "sage", ByPGSage, "", "", 0, true},
		{"psql", "sage_agentb_k2m4q7x9ab", ByRole, "", "sage_agentb_k2m4q7x9ab", 0, true},
		{"psql", "sage_agent_k2m4q7x9ab", ByRole, "", "sage_agent_k2m4q7x9ab", 0, true},
		{"pg_sage agent:", "app_user", "", "", "", 0, false},
		{"pg_sage agent:Bad Name:1", "app_user", "", "", "", 0, false},
		{"pg_sage agent:ci-bot:notanumber", "app_user", "", "", "", 0, false},
		{"psql", "sage_agent_short", "", "", "", 0, false},
		{"psql", "postgres", "", "", "", 0, false},
	}
	for _, c := range cases {
		a, ok := Attribute(entry(c.app, c.user, ddl))
		if ok != c.ok || a.By != c.by || a.Principal != c.principal || a.Role != c.role ||
			a.ActionID != c.action {
			t.Fatalf("app %q user %q: got %+v ok=%v", c.app, c.user, a, ok)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
