//go:build cgo

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
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func cell(t *testing.T, r Result, row, col int) string {
	t.Helper()
	if row >= len(r.Rows) || col >= len(r.Rows[row]) {
		t.Fatalf("no cell (%d,%d) in %+v", row, col, r.Rows)
	}
	v := r.Rows[row][col]
	if v == nil {
		return "<null>"
	}
	return *v
}

func TestQueryHappyPath(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res := f.query(t, "SELECT id, name FROM items WHERE id <= $1 ORDER BY id", "3")
	if res.Verdict != VerdictExecute || res.Status != StatusOK {
		t.Fatalf("result = %+v, want executed ok", res)
	}
	if res.RowCount != 3 || len(res.Rows) != 3 || res.Truncated {
		t.Errorf("rows = %d (%d), truncated %v; want 3", res.RowCount, len(res.Rows),
			res.Truncated)
	}
	if len(res.Columns) != 2 || res.Columns[0].Name != "id" || res.Columns[0].Type != "integer" ||
		res.Columns[1].Type != "text" {
		t.Errorf("columns = %+v", res.Columns)
	}
	if cell(t, res, 2, 1) != "item-3" {
		t.Errorf("row 3 name = %q", cell(t, res, 2, 1))
	}
	if len(res.EnvelopeHash) != 64 {
		t.Errorf("envelope hash %q", res.EnvelopeHash)
	}
	req := f.decider.last()
	if req.Kind != agentguard.ToolAgent || req.Capability != decide.CapRead ||
		req.Tool != "agent_query" || req.Database != "db" || req.PrincipalID != f.p.ID ||
		req.TaskID != "task-1" {
		t.Errorf("decide request = %+v", req)
	}
	if len(req.Objects) != 1 || req.Objects[0] != (decide.Object{Schema: f.schema,
		Relation: "items"}) {
		t.Errorf("decide objects = %+v, want the resolved %s.items", req.Objects, f.schema)
	}
	audit := f.auditRows(t)
	if len(audit) != 1 || audit[0].verdict != VerdictExecute || audit[0].rowCount != 3 ||
		audit[0].envelope != res.EnvelopeHash || audit[0].task != "task-1" {
		t.Errorf("audit = %+v", audit)
	}
}

// S0: the statement runs in a session logged in as the broker role, in a
// read-only transaction, never through SET ROLE on pg_sage's session.
func TestQueryRunsAsBrokerRoleReadOnly(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res := f.query(t, "SELECT current_user::text, session_user::text, "+
		"current_setting('transaction_read_only'), current_setting('application_name'), "+
		"current_setting('search_path')")
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	if cell(t, res, 0, 0) != f.role || cell(t, res, 0, 1) != f.role {
		t.Errorf("current/session user = %s/%s, want %s", cell(t, res, 0, 0),
			cell(t, res, 0, 1), f.role)
	}
	if cell(t, res, 0, 2) != "on" {
		t.Errorf("transaction_read_only = %s", cell(t, res, 0, 2))
	}
	if cell(t, res, 0, 3) != "pg_sage agent:"+f.p.Name {
		t.Errorf("application_name = %s", cell(t, res, 0, 3))
	}
	if want := "pg_catalog, " + f.schema + ", pg_temp"; cell(t, res, 0, 4) != want {
		t.Errorf("search_path = %s, want %s", cell(t, res, 0, 4), want)
	}
}

func TestQueryRowLimitAndTruncation(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res, err := f.broker.Query(f.ctx(), Request{Database: "db",
		SQL: "SELECT id FROM items ORDER BY id", MaxRows: 2})
	require.NoError(t, err)
	if len(res.Rows) != 2 || !res.Truncated || res.RowCount != 2 {
		t.Errorf("max_rows 2 over 5 rows = %d rows truncated %v", len(res.Rows), res.Truncated)
	}
	res, err = f.broker.Query(f.ctx(), Request{Database: "db",
		SQL: "SELECT id FROM items ORDER BY id", MaxRows: 5})
	require.NoError(t, err)
	if len(res.Rows) != 5 || res.Truncated {
		t.Errorf("max_rows == row count = %d rows truncated %v; want 5, false",
			len(res.Rows), res.Truncated)
	}
}

func TestQueryByteCap(t *testing.T) {
	f := newFixture(t, envbind.EnvProd, func(c *Config) { c.MaxBytes = 20 })
	res := f.query(t, "SELECT name FROM items ORDER BY id") // 6 bytes per row
	if !res.Truncated || len(res.Rows) != 3 {
		t.Errorf("byte cap 20 over 6-byte rows = %d rows truncated %v; want 3, true",
			len(res.Rows), res.Truncated)
	}
}

// RO-15 and its neighbours: the broker sets its own statement timeout in
// every transaction, so an agent cannot lift it. A SET is refused before
// execution, set_config is refused by the proof, and a long query is
// cancelled at the broker's bound.
func TestQueryEnforcesItsOwnStatementTimeout(t *testing.T) {
	f := newFixture(t, envbind.EnvProd, func(c *Config) {
		c.StatementTimeout = 300 * time.Millisecond
	})
	res := f.query(t, "SET statement_timeout = 0")
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonUnsupportedShape {
		t.Errorf("SET = %+v, want blocked %s", res, ReasonUnsupportedShape)
	}
	res = f.query(t, "SELECT set_config('statement_timeout', '0', true)")
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonNotProven {
		t.Errorf("set_config = %+v, want blocked %s", res, ReasonNotProven)
	}
	res = f.query(t, "SELECT current_setting('statement_timeout')")
	if res.Status != StatusOK || cell(t, res, 0, 0) != "300ms" {
		t.Errorf("statement_timeout inside the broker = %+v, want 300ms", res)
	}
	start := time.Now()
	res = f.query(t, "SELECT count(*) FROM generate_series(1, 500000000)")
	if res.Status != StatusFailed || res.SQLState != "57014" ||
		!strings.Contains(res.Message, "300ms") {
		t.Errorf("long query = %+v, want failed 57014 naming the bound", res)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("long query took %v, the broker did not bound it", took)
	}
}

func TestQueryRefusalsBeforeExecution(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	cases := []struct{ sql, reason string }{
		{"SELECT 1; DELETE FROM items", ReasonUnsupportedShape},
		{"INSERT INTO items SELECT 99, 'x'", ReasonUnsupportedShape},
		{"WITH d AS (DELETE FROM items RETURNING id) SELECT * FROM d", ReasonUnsupportedShape},
		{"SELECT * FROM items FOR UPDATE", ReasonUnsupportedShape},
		{"SELECT pg_sleep(0)", ReasonNotProven},
		{"SELECT pg_advisory_lock(1)", ReasonNotProven},
		{"SELECT proname FROM pg_proc", ReasonNotProven},
		{"VALUES (nextval('" + "x" + "'))", ReasonNotProven},
		{"SELECT 1 FROM items WHERE id = $1", ""}, // no parameter given: invalid
	}
	for _, c := range cases {
		res, err := f.broker.Query(f.ctx(), Request{Database: "db", SQL: c.sql})
		if c.reason == "" {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%q: error = %v, want ErrInvalid (parameter count)", c.sql, err)
			}
			continue
		}
		require.NoError(t, err)
		if res.Verdict != VerdictBlocked || res.ReasonCode != c.reason {
			t.Errorf("%q: %+v, want blocked %s", c.sql, res, c.reason)
		}
	}
	var n int
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT count(*) FROM "+ident(f.schema)+".items").Scan(&n))
	if n != 5 {
		t.Errorf("items has %d rows after the refusals, want 5", n)
	}
}

func TestQueryPrivilegeErrorIsSanitized(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res := f.query(t, "SELECT id FROM hidden")
	if res.Verdict != VerdictExecute || res.Status != StatusFailed || res.SQLState != "42501" {
		t.Fatalf("ungranted table = %+v, want failed 42501", res)
	}
	if strings.Contains(res.Message, "hidden") {
		t.Errorf("message %q echoes server text", res.Message)
	}
	res = f.query(t, "SELECT name::int FROM items")
	if res.SQLState != "22P02" || strings.Contains(res.Message, "item-") {
		t.Errorf("cast error = %+v, want 22P02 without the value", res)
	}
	audit := f.auditRows(t)
	if len(audit) != 2 || audit[0].reason != "sqlstate_42501" {
		t.Errorf("audit = %+v, want the failure recorded by SQLSTATE", audit)
	}
}

func TestQueryRowLevelSecurityApplies(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res := f.query(t, "SELECT id FROM owned ORDER BY id")
	if res.Status != StatusOK || len(res.Rows) != 1 || cell(t, res, 0, 0) != "2" {
		t.Errorf("RLS table = %+v, want only the agent's own row", res)
	}
}

func TestQueryMasksInDevAndDeniesInProd(t *testing.T) {
	f := newFixture(t, envbind.EnvDev)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	res := f.query(t, "SELECT id, ssn FROM people ORDER BY id")
	if res.Status != StatusOK || cell(t, res, 0, 1) != MaskedValue ||
		cell(t, res, 0, 0) != "1" {
		t.Fatalf("dev pii = %+v, want ssn masked", res)
	}
	if len(res.Masked) != 1 || res.Masked[0] != "ssn" || res.Columns[1].Class != "pii" {
		t.Errorf("masked = %v, columns = %+v", res.Masked, res.Columns)
	}
	// An expression over a classified relation counts as classified (S5).
	res = f.query(t, "SELECT upper(name) FROM people")
	if res.Status != StatusOK || cell(t, res, 0, 0) != MaskedValue {
		t.Errorf("expression over a pii relation = %+v, want masked", res)
	}
	// G1-15: a cast on a masked column leaks through its error; refused.
	res = f.query(t, "SELECT id FROM people WHERE ssn::int = 0")
	if res.Verdict != VerdictBlocked || res.ReasonCode != string(decide.ReasonClassification) {
		t.Errorf("masked cast = %+v, want blocked agent_classification", res)
	}
	res = f.query(t, "SELECT id FROM people p WHERE p::text LIKE '%123%'")
	if res.Verdict != VerdictBlocked {
		t.Errorf("whole-row filter = %+v, want blocked", res)
	}
	res = f.query(t, "SELECT id, ssn FROM people_view ORDER BY id")
	if res.Status != StatusOK || cell(t, res, 0, 1) != MaskedValue {
		t.Errorf("view over pii in dev = %+v, want masked", res)
	}

	f.decider.verdict.Env = envbind.EnvProd
	res = f.query(t, "SELECT id, ssn FROM people")
	if res.Verdict != VerdictBlocked || res.ReasonCode != string(decide.ReasonClassification) {
		t.Errorf("prod pii = %+v, want blocked", res)
	}
	res = f.query(t, "SELECT id, ssn FROM people_view")
	if res.Verdict != VerdictBlocked {
		t.Errorf("prod view over pii = %+v, want blocked", res)
	}
	res = f.query(t, "SELECT id FROM people ORDER BY id")
	if res.Status != StatusOK || len(res.Rows) != 2 {
		t.Errorf("prod clean column of a pii table = %+v, want allowed", res)
	}
}

func TestQueryUnmaskEntryAllowsPII(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	b, err := New(f.cfg, Deps{Targets: fakeTargets{"db": f.target}, Logins: f.logins,
		Decider: f.decider, Classes: StoreClasses{}, Audit: &memAudit{},
		Unmasked: func(pid, dbid string, col classify.Column) bool {
			return pid == f.p.ID && dbid == f.target.DatabaseID && col.Name == "ssn"
		}})
	require.NoError(t, err)
	defer b.Close()
	res, err := b.Query(f.ctx(), Request{Database: "db", SQL: "SELECT ssn FROM people " +
		"ORDER BY id"})
	require.NoError(t, err)
	if res.Status != StatusOK || cell(t, res, 0, 0) != "123-45-6789" || len(res.Masked) != 0 {
		t.Errorf("unmasked pii = %+v", res)
	}
}

func TestQuerySecretDeniedEverywhere(t *testing.T) {
	f := newFixture(t, envbind.EnvBranch)
	f.classifyColumn(t, "people", "api_key", classify.ClassSecret)
	res := f.query(t, "SELECT api_key FROM people")
	if res.Verdict != VerdictBlocked || res.ReasonCode != string(decide.ReasonClassification) {
		t.Errorf("secret in branch = %+v, want blocked", res)
	}
	audit := f.auditRows(t)
	if len(audit) != 1 || len(audit[0].classes) != 1 || audit[0].classes[0] != "secret" {
		t.Errorf("audit = %+v, want the secret class recorded", audit)
	}
}

func TestQueryDecisionBlocksAndIsAudited(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.decider.verdict = decide.Verdict{Reason: agentguard.ReasonFrozen, Step: "D1",
		Detail: "frozen by the operator", Fix: "an operator unfreezes the agent"}
	res := f.query(t, "SELECT id FROM items")
	if res.Verdict != VerdictBlocked || res.ReasonCode != "agent_frozen" || len(res.Rows) != 0 {
		t.Errorf("frozen = %+v", res)
	}
	if f.logins.calls != 0 {
		t.Error("a broker login was opened for a denied request")
	}
	audit := f.auditRows(t)
	if len(audit) != 1 || audit[0].verdict != VerdictBlocked ||
		audit[0].reason != "agent_frozen" || audit[0].step != "D1" {
		t.Errorf("audit = %+v, want blocked agent_frozen at D1", audit)
	}
}

func TestQueryLoginFailures(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	cases := []struct {
		err    error
		reason string
	}{
		{fmt.Errorf("wrapped: %w", agentguard.ErrNotFound), ReasonNoRole},
		{agentguard.ErrEncryptionKeyRequired, ReasonEncryptionKey},
		{&agentguard.DeniedError{Reason: agentguard.ReasonFrozen, Detail: "killed"},
			"agent_frozen"},
	}
	for _, c := range cases {
		f.logins.set(f.pw, c.err)
		res := f.query(t, "SELECT 1")
		if res.Verdict != VerdictBlocked || res.ReasonCode != c.reason {
			t.Errorf("%v: %+v, want blocked %s", c.err, res, c.reason)
		}
	}
	f.logins.set(f.pw, errors.New("control database down"))
	_, err := f.broker.Query(f.ctx(), Request{Database: "db", SQL: "SELECT 1"})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("login store down: error = %v, want ErrUnavailable", err)
	}
}

// A rotated credential replaces the cached pool on the next call.
func TestQueryFollowsCredentialRotation(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	if res := f.query(t, "SELECT 1"); res.Status != StatusOK {
		t.Fatalf("first query = %+v", res)
	}
	exec(t, f.super, fmt.Sprintf("ALTER ROLE %s PASSWORD 'rotated-pw'", ident(f.role)))
	f.logins.set("rotated-pw", nil)
	if res := f.query(t, "SELECT 2"); res.Status != StatusOK {
		t.Errorf("after rotation = %+v, want the new credential used", res)
	}
}

func TestQueryWithoutAuditReturnsNoRows(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b := f.newBroker(t, &memAudit{fail: errAuditDown})
	res, err := b.Query(f.ctx(), Request{Database: "db", SQL: "SELECT id FROM items"})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("rows returned without an audit row: %+v", res.Rows)
	}
}

// The broker keeps within the role's connection limit and its own budget
// under concurrent calls, and Evict closes the principal's sessions.
func TestQueryConcurrentAndEvict(t *testing.T) {
	f := newFixture(t, envbind.EnvProd, func(c *Config) { c.MaxTotalConns = 2 })
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := f.broker.Query(f.ctx(), Request{Database: "db",
				SQL: "SELECT id FROM items WHERE id = $1", Params: []any{fmt.Sprint(i%5 + 1)}})
			if err != nil || res.Status != StatusOK || len(res.Rows) != 1 {
				errs <- fmt.Sprintf("call %d: %+v %v", i, res, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := len(f.auditRows(t)); n != 16 {
		t.Errorf("%d audit rows, want 16", n)
	}
	f.broker.Evict(f.p.ID)
	// A closed session leaves pg_stat_activity when its backend exits.
	sessions := -1
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		require.NoError(t, f.super.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE usename = $1", f.role).Scan(&sessions))
		if sessions == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sessions != 0 {
		t.Errorf("%d broker sessions after Evict, want 0", sessions)
	}
}
