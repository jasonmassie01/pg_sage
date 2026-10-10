package executor

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rolegrants"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The startup grants check depends on the trust level: observation is
// read-only and needs neither CREATE on the schemas holding tables nor
// pg_signal_backend, so their absence is one INFO line pointing at the
// Grant more guide; advisory and autonomous run DDL and cancel queries, so
// a missing grant is a WARNING with the SQL that fixes it.

// No concurrent access tests: reportGrants is stateless and VerifyGrants
// shares nothing between calls.

type logLine struct{ component, text string }

func captureLog(lines *[]logLine) func(string, string, ...any) {
	return func(component, msg string, args ...any) {
		*lines = append(*lines, logLine{component, fmt.Sprintf(msg, args...)})
	}
}

// publicLacking: user tables live in public and app; CREATE is missing on
// public only.
var publicLacking = rolegrants.SchemaCreate{Schemas: []string{"app", "public"},
	Missing: []string{"public"}}

func bothMissing() []missingGrant {
	return []missingGrant{schemaCreateGrant("sage_agent", publicLacking),
		signalBackendGrant("sage_agent")}
}

func TestReportGrantsObservationIsOneInfoLine(t *testing.T) {
	var lines []logLine
	reportGrants("observation", "sage_agent", bothMissing(), captureLog(&lines))
	if len(lines) != 1 {
		t.Fatalf("lines = %+v, want exactly one", lines)
	}
	l := lines[0]
	if l.component != "grants" || strings.Contains(l.text, "WARNING") ||
		!strings.Contains(l.text, "Grant more") || !strings.Contains(l.text, "observation") ||
		!strings.Contains(l.text, "pg_signal_backend") ||
		!strings.Contains(l.text, "CREATE on schema public") ||
		!strings.Contains(l.text, `"sage_agent"`) {
		t.Fatalf("line = %+v", l)
	}
}

func TestReportGrantsExecutingLevelsWarnWithFix(t *testing.T) {
	for _, level := range []string{"advisory", "autonomous"} {
		var lines []logLine
		reportGrants(level, "sage_agent", bothMissing(), captureLog(&lines))
		if len(lines) != 2 {
			t.Fatalf("%s: lines = %+v, want one warning per missing grant", level, lines)
		}
		for i, fix := range []string{"GRANT CREATE ON SCHEMA public TO sage_agent",
			"GRANT pg_signal_backend TO sage_agent"} {
			l := lines[i]
			if l.component != "grants" || !strings.HasPrefix(l.text, "WARNING:") ||
				!strings.Contains(l.text, "trust "+level) || !strings.Contains(l.text, fix) {
				t.Fatalf("%s line %d = %+v, want a warning with %q", level, i, l, fix)
			}
		}
	}
}

// Empty: nothing missing logs nothing at any level.
func TestReportGrantsNothingMissingIsSilent(t *testing.T) {
	for _, level := range []string{"observation", "advisory", "autonomous", ""} {
		var lines []logLine
		reportGrants(level, "sage_agent", nil, captureLog(&lines))
		if len(lines) != 0 {
			t.Fatalf("%q: lines = %+v, want none", level, lines)
		}
	}
}

// Invalid/zero level: anything that is not an executing level is treated
// as read-only (no warnings), the way the executor treats it.
func TestReportGrantsUnknownLevelIsReadOnly(t *testing.T) {
	for _, level := range []string{"", "monitor", "ADVISORY"} {
		var lines []logLine
		reportGrants(level, "sage_agent", bothMissing(), captureLog(&lines))
		if len(lines) != 1 || strings.Contains(lines[0].text, "WARNING") {
			t.Fatalf("%q: lines = %+v, want one info line", level, lines)
		}
	}
}

func TestExecutesActions(t *testing.T) {
	for level, want := range map[string]bool{"observation": false, "advisory": true,
		"autonomous": true, "": false, "monitor": false} {
		if got := executesActions(level); got != want {
			t.Errorf("executesActions(%q) = %v, want %v", level, got, want)
		}
	}
}

// grantsRole is a login role in the database at dsn with no privileges
// beyond CONNECT; it is never a member of pg_signal_backend.
func grantsRole(t *testing.T, dsn string) (*pgxpool.Pool, string) {
	t.Helper()
	return grantsRoleNamed(t, dsn, fmt.Sprintf("grants_%06x", time.Now().UnixNano()&0xffffff))
}

// grantsRoleNamed is grantsRole with the role name chosen by the caller.
func grantsRoleNamed(t *testing.T, dsn, role string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	rq := pgx.Identifier{role}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+rq+" LOGIN PASSWORD 'pw_"+role+"'"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		// DROP OWNED revokes what a test granted, which would block DROP ROLE.
		_, _ = admin.Exec(context.Background(), "DROP OWNED BY "+rq)
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+rq)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, "pw_"+role)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool, role
}

// tablesInApp is a fresh database whose only user table is app.t.
func tablesInApp(t *testing.T) string {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "grants_level")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA app; CREATE TABLE app.t (a int)"); err != nil {
		t.Fatalf("create app.t: %v", err)
	}
	return dsn
}

// Integration: a fresh role lacks pg_signal_backend and CREATE on app, the
// schema holding the database's only table. The level decides whether that
// is a warning. (Before, the check read CREATE on public only, whatever
// schemas held the tables pg_sage indexes.)
func TestVerifyGrantsDependsOnTrustLevel(t *testing.T) {
	pool, role := grantsRole(t, tablesInApp(t))
	ctx := context.Background()
	var obs []logLine
	VerifyGrants(ctx, pool, "ignored", "observation", captureLog(&obs))
	if len(obs) != 1 || strings.Contains(obs[0].text, "WARNING") ||
		!strings.Contains(obs[0].text, role) || !strings.Contains(obs[0].text, "Grant more") ||
		!strings.Contains(obs[0].text, "CREATE on schema app") {
		t.Fatalf("observation: lines = %+v, want one info line naming %s", obs, role)
	}
	for _, level := range []string{"advisory", "autonomous"} {
		var lines []logLine
		VerifyGrants(ctx, pool, "ignored", level, captureLog(&lines))
		if len(lines) != 2 || !strings.HasPrefix(lines[0].text, "WARNING:") ||
			!strings.Contains(lines[0].text, "GRANT CREATE ON SCHEMA app TO "+role) ||
			!strings.HasPrefix(lines[1].text, "WARNING:") ||
			!strings.Contains(lines[1].text, "GRANT pg_signal_backend TO "+role) {
			t.Fatalf("%s: lines = %+v, want the two warnings with their fixes", level, lines)
		}
	}
}

// Error propagation: a failed privilege query is logged with what was
// attempted, and no grant is reported missing on a guess.
func TestVerifyGrantsQueryErrorIsLogged(t *testing.T) {
	pool, _ := grantsRole(t, testdb.SkipUnlessLive(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var lines []logLine
	VerifyGrants(ctx, pool, "sage_agent", "advisory", captureLog(&lines))
	if len(lines) != 2 {
		t.Fatalf("lines = %+v, want one error per check", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l.text, "could not check") || strings.Contains(l.text, "WARNING") {
			t.Fatalf("line = %+v, want a could-not-check error", l)
		}
	}
}
