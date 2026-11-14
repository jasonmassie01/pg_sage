package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// These tests run with and without cgo: without it the broker reads the
// statements below from a script (parser_script_test.go), so the whole
// execution path (login, transaction, proof, output plan, cursor, audit)
// still runs against a real server.
const (
	sqlUpTo    = "SELECT id, name FROM items WHERE id <= $1 ORDER BY id"
	sqlNames   = "SELECT name FROM items ORDER BY id"
	sqlByID    = "SELECT id FROM items WHERE id = $1"
	sqlSlow    = "SELECT count(*) FROM generate_series(1, 500000000)"
	sqlSleep   = "SELECT pg_sleep(3)"
	sqlMissing = "SELECT id FROM no_such_relation"
	sqlDelete  = "DELETE FROM items"
	sqlMulti   = "SELECT 1; SELECT 2"
)

var errNotOneSelect = errors.New("scripted: not exactly one SELECT statement")

var pathStatements = map[string]scripted{
	sqlUpTo: selectRead(sqlUpTo, 1, []string{"items"}, nil, []string{"id", "name"},
		[]string{"id"}),
	sqlNames: selectRead(sqlNames, 0, []string{"items"}, nil, []string{"name"},
		[]string{"id"}),
	sqlByID: selectRead(sqlByID, 1, []string{"items"}, nil, []string{"id"},
		[]string{"id"}),
	sqlSlow:    selectRead(sqlSlow, 0, nil, []string{"count", "generate_series"}, nil, nil),
	sqlSleep:   selectRead(sqlSleep, 0, nil, []string{"pg_sleep"}, nil, nil),
	sqlMissing: selectRead(sqlMissing, 0, []string{"no_such_relation"}, nil, []string{"id"}, nil),
	sqlDelete:  {err: errNotOneSelect},
	sqlMulti:   {err: errNotOneSelect},
}

// logSink keeps the broker's operator log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// pathBroker is a broker over the fixture with the real audit table, a
// captured operator log and the scripted statements.
func (f *fixture) pathBroker(t *testing.T) (*Broker, *logSink) {
	t.Helper()
	logs := &logSink{}
	b, err := New(f.cfg, Deps{Targets: fakeTargets{"db": f.target}, Logins: f.logins,
		Decider: f.decider, Classes: StoreClasses{}, Audit: &SQLAudit{}, Log: logs.log})
	require.NoError(t, err)
	t.Cleanup(b.Close)
	useScriptedParser(b, pathStatements)
	return b, logs
}

func (f *fixture) ask(b *Broker, sql string, maxRows int, params ...any) (Result, error) {
	return b.Query(f.ctx(), Request{Database: "db", SQL: sql, MaxRows: maxRows,
		Params: params})
}

func (f *fixture) loginCalls() int {
	f.logins.mu.Lock()
	defer f.logins.mu.Unlock()
	return f.logins.calls
}

func (f *fixture) setVerdict(v decide.Verdict) {
	f.decider.mu.Lock()
	defer f.decider.mu.Unlock()
	f.decider.verdict = v
}

// roleSessions counts the broker role's backends, waiting up to 3s for
// closed ones to leave pg_stat_activity.
func (f *fixture) roleSessions(t *testing.T) int {
	t.Helper()
	n := -1
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		require.NoError(t, f.super.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE usename = $1", f.role).Scan(&n))
		if n == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

func TestPathQueryAuditsPrincipalAndFingerprint(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b, _ := f.pathBroker(t)
	res, err := f.ask(b, sqlUpTo, 0, "3")
	require.NoError(t, err)
	if res.Verdict != VerdictExecute || res.Status != StatusOK || res.RowCount != 3 ||
		res.Truncated || cell(t, res, 0, 0) != "1" || cell(t, res, 2, 1) != "item-3" {
		t.Fatalf("result = %+v, want rows 1..3 of items", res)
	}
	read, err := b.parse.brokered(sqlUpTo)
	require.NoError(t, err)
	var principal, fingerprint, role, verdict, envelope, dbID string
	var rows int
	require.NoError(t, f.super.QueryRow(context.Background(), `SELECT principal_id,
		coalesce(fingerprint, ''), coalesce(broker_role, ''), verdict, row_count,
		envelope_hash, database_id::text FROM sage.guard_query_audit
		WHERE principal_id = $1`, f.p.ID).Scan(&principal, &fingerprint, &role, &verdict,
		&rows, &envelope, &dbID))
	if principal != f.p.ID || fingerprint == "" || fingerprint != read.Fingerprint {
		t.Errorf("audit principal %q fingerprint %q, want %q and %q", principal,
			fingerprint, f.p.ID, read.Fingerprint)
	}
	if role != f.role || verdict != VerdictExecute || rows != 3 ||
		envelope != res.EnvelopeHash || dbID != f.target.DatabaseID {
		t.Errorf("audit = role %q verdict %q rows %d envelope %q db %q", role, verdict,
			rows, envelope, dbID)
	}
}

// Writes, multiple statements and empty SQL never reach a broker login;
// a denied function is refused by the catalog proof, before it runs.
func TestPathRefusalsSendNothing(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b, _ := f.pathBroker(t)
	for _, sql := range []string{sqlDelete, sqlMulti} {
		res, err := f.ask(b, sql, 0)
		require.NoError(t, err)
		if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonUnsupportedShape ||
			len(res.Rows) != 0 {
			t.Errorf("%q = %+v, want blocked %s", sql, res, ReasonUnsupportedShape)
		}
	}
	for _, sql := range []string{"", "  \n\t"} {
		if _, err := f.ask(b, sql, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: error = %v, want ErrInvalid", sql, err)
		}
	}
	if n := f.loginCalls(); n != 0 {
		t.Errorf("%d broker logins for refused statements, want 0", n)
	}
	start := time.Now()
	res, err := f.ask(b, sqlSleep, 0)
	require.NoError(t, err)
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonNotProven ||
		!strings.Contains(res.Detail, "pg_sleep") {
		t.Errorf("pg_sleep = %+v, want blocked %s naming it", res, ReasonNotProven)
	}
	if took := time.Since(start); took >= 3*time.Second {
		t.Errorf("pg_sleep(3) refusal took %v: the statement ran", took)
	}
	var n int
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT count(*) FROM "+ident(f.schema)+".items").Scan(&n))
	audit := f.auditRows(t)
	if n != 5 || len(audit) != 3 {
		t.Fatalf("items = %d, audit rows = %d; want 5 and 3 (empty SQL is not audited)",
			n, len(audit))
	}
	for _, a := range audit {
		if a.verdict != VerdictBlocked || a.rowCount != -1 {
			t.Errorf("audit row %+v, want blocked without a row count", a)
		}
	}
}

// No audit, no rows: a sink that fails returns nothing, and a broker
// without a sink cannot be built.
func TestPathAuditSinkFailsClosed(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	_, err := New(f.cfg, Deps{Targets: fakeTargets{"db": f.target}, Logins: f.logins,
		Decider: f.decider, Classes: StoreClasses{}})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("New without an audit sink: error = %v, want ErrInvalid", err)
	}
	wrapped := fmt.Errorf("%w: audit replica lagging", ErrUnavailable)
	for _, sinkErr := range []error{errAuditDown, wrapped} {
		b := f.newBroker(t, &memAudit{fail: sinkErr})
		useScriptedParser(b, pathStatements)
		res, err := f.ask(b, sqlUpTo, 0, "5")
		if !errors.Is(err, ErrUnavailable) || len(res.Rows) != 0 || res.Verdict != "" {
			t.Errorf("sink %v: result %+v error %v, want nothing and ErrUnavailable",
				sinkErr, res, err)
		}
		if sinkErr == wrapped && err != nil && err.Error() != wrapped.Error() {
			t.Errorf("an unavailable sink's error was rewrapped: %q", err)
		}
	}
}

func TestPathRowCapBoundary(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b, _ := f.pathBroker(t)
	res, err := f.ask(b, sqlNames, 5)
	require.NoError(t, err)
	if len(res.Rows) != 5 || res.RowCount != 5 || res.Truncated {
		t.Errorf("max_rows 5 over 5 rows = %d rows truncated %v; want 5, false",
			len(res.Rows), res.Truncated)
	}
	res, err = f.ask(b, sqlNames, 4)
	require.NoError(t, err)
	if len(res.Rows) != 4 || res.RowCount != 4 || !res.Truncated ||
		cell(t, res, 3, 0) != "item-4" {
		t.Errorf("max_rows 4 over 5 rows = %+v; want 4 rows, truncated", res)
	}
	audit := f.auditRows(t)
	if len(audit) != 2 || audit[0].rowCount != 5 || audit[1].rowCount != 4 {
		t.Errorf("audit = %+v, want row counts 5 then 4", audit)
	}
}

// Each name is 6 bytes ("item-1"): 30 bytes hold all five rows, 29 hold
// four.
func TestPathByteCapBoundary(t *testing.T) {
	f := newFixture(t, envbind.EnvProd, func(c *Config) { c.MaxBytes = 30 })
	b, _ := f.pathBroker(t)
	res, err := f.ask(b, sqlNames, 0)
	require.NoError(t, err)
	if len(res.Rows) != 5 || res.Truncated {
		t.Errorf("byte cap 30 = %d rows truncated %v; want 5, false", len(res.Rows),
			res.Truncated)
	}
	f.cfg.MaxBytes = 29
	b, _ = f.pathBroker(t)
	res, err = f.ask(b, sqlNames, 0)
	require.NoError(t, err)
	if len(res.Rows) != 4 || !res.Truncated || res.RowCount != 4 {
		t.Errorf("byte cap 29 = %d rows truncated %v; want 4, true", len(res.Rows),
			res.Truncated)
	}
}

// The broker's statement timeout cancels a long read, which the agent sees
// as a sanitized failure; an unknown relation is refused by the proof.
func TestPathTimeoutAndUnknownRelation(t *testing.T) {
	f := newFixture(t, envbind.EnvProd, func(c *Config) {
		c.StatementTimeout = 300 * time.Millisecond
	})
	b, logs := f.pathBroker(t)
	start := time.Now()
	res, err := f.ask(b, sqlSlow, 0)
	require.NoError(t, err)
	if res.Status != StatusFailed || res.SQLState != "57014" || res.Retryable ||
		!strings.Contains(res.Message, "300ms") || len(res.Rows) != 0 {
		t.Errorf("long read = %+v, want failed 57014 naming 300ms", res)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("long read took %v", took)
	}
	if !strings.Contains(logs.text(), "SQLSTATE 57014") {
		t.Errorf("operator log lacks the cancellation: %q", logs.text())
	}
	res, err = f.ask(b, sqlMissing, 0)
	require.NoError(t, err)
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonNotProven ||
		!strings.Contains(res.Detail, "unknown relation") {
		t.Errorf("unknown relation = %+v, want blocked %s", res, ReasonNotProven)
	}
	audit := f.auditRows(t)
	if len(audit) != 2 || audit[0].reason != "sqlstate_57014" ||
		audit[1].reason != ReasonNotProven {
		t.Errorf("audit = %+v, want sqlstate_57014 then %s", audit, ReasonNotProven)
	}
}

// A broker login that fails after the credential rotated names the role,
// never the password, drops the pool, and recovers once the role can log
// in with the new credential.
func TestPathLoginFailureAfterRotation(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b, logs := f.pathBroker(t)
	if res, err := f.ask(b, sqlByID, 0, "1"); err != nil || res.Status != StatusOK {
		t.Fatalf("first read = %+v %v", res, err)
	}
	rotated := "rotated-" + f.p.ID[4:16]
	f.logins.set(rotated, nil)
	exec(t, f.super, fmt.Sprintf("ALTER ROLE %s NOLOGIN PASSWORD '%s'", ident(f.role),
		rotated))
	res, err := f.ask(b, sqlByID, 0, "2")
	if !errors.Is(err, ErrUnavailable) || len(res.Rows) != 0 {
		t.Fatalf("login refused: %+v %v, want ErrUnavailable and no rows", res, err)
	}
	for _, secret := range []string{rotated, f.pw} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.text(), secret) {
			t.Errorf("a password reached the error or the log: %q / %q", err, logs.text())
		}
	}
	if !strings.Contains(err.Error(), f.role) {
		t.Errorf("error %q does not name the broker role %s", err, f.role)
	}
	b.pools.mu.Lock()
	pooled := len(b.pools.byKey)
	b.pools.mu.Unlock()
	if pooled != 0 || len(b.pools.budget) != 0 {
		t.Errorf("after a refused login: %d pools, %d budget slots held; want 0, 0", pooled,
			len(b.pools.budget))
	}
	exec(t, f.super, fmt.Sprintf("ALTER ROLE %s LOGIN", ident(f.role)))
	res, err = f.ask(b, sqlByID, 0, "2")
	if err != nil || res.Status != StatusOK || cell(t, res, 0, 0) != "2" {
		t.Errorf("after the role can log in again = %+v %v, want row 2", res, err)
	}
}

// The kill switch between two reads: when the gate has the freeze the
// second read is refused without a login; when only the role kill has
// landed, the second read fails closed and no session survives.
func TestPathKillBetweenTwoQueries(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b, _ := f.pathBroker(t)
	if res, err := f.ask(b, sqlByID, 0, "1"); err != nil || res.Status != StatusOK {
		t.Fatalf("first read = %+v %v", res, err)
	}
	logins := f.loginCalls()
	allowed := f.decider.verdict
	f.setVerdict(decide.Verdict{Reason: agentguard.ReasonFrozen, Step: "D1",
		Detail: "frozen by the operator", Fix: "an operator unfreezes the agent"})
	res, err := f.ask(b, sqlByID, 0, "1")
	require.NoError(t, err)
	if res.Verdict != VerdictBlocked || res.ReasonCode != "agent_frozen" || len(res.Rows) != 0 {
		t.Errorf("read after the freeze = %+v, want blocked agent_frozen", res)
	}
	if f.loginCalls() != logins {
		t.Error("the frozen read opened a broker login")
	}
	f.setVerdict(allowed)
	exec(t, f.super, fmt.Sprintf("ALTER ROLE %s NOLOGIN CONNECTION LIMIT 0", ident(f.role)))
	exec(t, f.super, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
		"WHERE usename = '"+f.role+"'")
	for i := 0; i < 2; i++ {
		res, err = f.ask(b, sqlByID, 0, "1")
		if !errors.Is(err, ErrUnavailable) || len(res.Rows) != 0 {
			t.Errorf("read %d after the role kill = %+v %v, want ErrUnavailable", i, res, err)
		}
	}
	if n := f.roleSessions(t); n != 0 {
		t.Errorf("%d broker sessions after the kill, want 0", n)
	}
	audit := f.auditRows(t)
	if len(audit) != 2 || audit[1].reason != "agent_frozen" || audit[1].step != "D1" {
		t.Errorf("audit = %+v, want the read and the frozen refusal only", audit)
	}
}
