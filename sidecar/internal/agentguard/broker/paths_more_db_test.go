package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// 32 concurrent reads while the principal's pools are evicted: each read
// returns its own row or fails closed (ErrUnavailable, nothing returned),
// every returned read is audited, no budget slot leaks, and the broker
// recovers after the evictions.
func TestPathConcurrentReadsDuringEviction(t *testing.T) {
	f := newFixture(t, envbind.EnvProd, func(c *Config) { c.MaxTotalConns = 2 })
	b, _ := f.pathBroker(t)
	const n = 32
	ok, unavailable := f.readsDuringEviction(t, b, n)
	if ok+unavailable != n {
		t.Errorf("%d reads accounted for, want %d", ok+unavailable, n)
	}
	if audited := len(f.auditRows(t)); audited != ok {
		t.Errorf("%d audit rows for %d returned reads", audited, ok)
	}
	if len(b.pools.budget) != 0 {
		t.Fatalf("%d connection budget slots leaked", len(b.pools.budget))
	}
	res, err := f.ask(b, sqlByID, 0, "4")
	if err != nil || res.Status != StatusOK || cell(t, res, 0, 0) != "4" {
		t.Errorf("read after the evictions = %+v %v, want row 4", res, err)
	}
	b.Evict(f.p.ID)
	if s := f.roleSessions(t); s != 0 {
		t.Errorf("%d broker sessions after the final Evict, want 0", s)
	}
}

// readsDuringEviction runs n concurrent reads while another goroutine
// evicts the principal's pools 20 times; it counts reads that returned
// their row and reads that failed closed, and reports any other outcome.
// The reads share a deadline, so a leaked budget slot fails the test
// instead of hanging it.
func (f *fixture) readsDuringEviction(t *testing.T, b *Broker, n int) (int, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx(), 30*time.Second)
	defer cancel()
	var ok, unavailable atomic.Int32
	errs := make(chan string, n)
	var wg sync.WaitGroup
	evicted := make(chan struct{})
	go func() {
		defer close(evicted)
		for i := 0; i < 20; i++ {
			b.Evict(f.p.ID)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprint(i%5 + 1)
			res, err := b.Query(ctx, Request{Database: "db", SQL: sqlByID,
				Params: []any{want}})
			switch {
			case errors.Is(err, ErrUnavailable) && len(res.Rows) == 0:
				unavailable.Add(1)
			case err == nil && res.Status == StatusOK && len(res.Rows) == 1 &&
				res.Rows[0][0] != nil && *res.Rows[0][0] == want:
				ok.Add(1)
			default:
				errs <- fmt.Sprintf("read %d: %+v %v", i, res, err)
			}
		}(i)
	}
	wg.Wait()
	<-evicted
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	return int(ok.Load()), int(unavailable.Load())
}

// explain_query on the scripted path: a parameterised statement is
// prepared (with and without its value), a plain one is explained
// directly, nothing is fetched, and each plan is audited without a row
// count.
func TestPathExplainPlans(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	b, _ := f.pathBroker(t)
	cases := []ExplainRequest{
		{Database: "db", SQL: sqlByID, Params: []string{"2"}},
		{Database: "db", SQL: sqlByID},
		{Database: "db", SQL: sqlNames},
	}
	for _, req := range cases {
		res, err := b.Explain(f.ctx(), req)
		require.NoError(t, err)
		if res.Verdict != VerdictExecute || res.Status != StatusOK ||
			!strings.Contains(string(res.PlanJSON), `"Plan"`) || len(res.EnvelopeHash) != 64 {
			t.Errorf("explain %q %v = %+v, want a JSON plan", req.SQL, req.Params, res)
		}
	}
	res, err := b.Explain(f.ctx(), ExplainRequest{Database: "db", SQL: sqlMissing})
	require.NoError(t, err)
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonNotProven ||
		len(res.PlanJSON) != 0 {
		t.Errorf("explain of an unknown relation = %+v, want blocked", res)
	}
	audit := f.auditRows(t)
	if len(audit) != 4 {
		t.Fatalf("audit rows = %d, want 4", len(audit))
	}
	for _, a := range audit[:3] {
		if a.verdict != VerdictExecute || a.rowCount != -1 {
			t.Errorf("explain audit row %+v, want executed without a row count", a)
		}
	}
	var prepared int
	require.NoError(t, f.super.QueryRow(context.Background(), "SELECT count(*) FROM "+
		"pg_stat_activity WHERE usename = $1 AND state LIKE 'idle in transaction%'",
		f.role).Scan(&prepared))
	if prepared != 0 {
		t.Errorf("%d broker sessions left inside a transaction", prepared)
	}
}

// The attribution view after brokered reads: the audit pages newest
// first, and pg_stat_statements attributes the reads to the broker role.
func TestPathActivityAfterReads(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	exec(t, f.super, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	b, _ := f.pathBroker(t)
	for _, q := range []struct {
		sql    string
		params []any
	}{{sqlUpTo, []any{"2"}}, {sqlNames, nil}, {sqlSleep, nil}} {
		_, err := f.ask(b, q.sql, 0, q.params...)
		require.NoError(t, err)
	}
	req := ActivityRequest{PrincipalID: f.p.ID, From: time.Now().Add(-time.Hour),
		To: time.Now().Add(time.Minute), Limit: 2}
	a, err := ReadActivity(context.Background(), f.target, req)
	require.NoError(t, err)
	if len(a.Queries) != 2 || a.Queries[0].Verdict != VerdictBlocked || a.NextCursor == "" {
		t.Fatalf("first page = %+v (cursor %q), want 2 entries, blocked first", a.Queries,
			a.NextCursor)
	}
	if len(a.Roles) != 2 || a.Roles[0] != f.role ||
		a.Roles[1] != agentguard.LoginRoleName(f.p.ID) {
		t.Errorf("roles = %v", a.Roles)
	}
	if a.Attribution.AuditedExecutions != 2 || a.Attribution.AttributedCalls < 2 ||
		a.Attribution.Source != SourcePSS || !a.Attribution.Complete {
		t.Errorf("attribution = %+v, want 2 audited and attributed", a.Attribution)
	}
	for _, s := range a.Statements {
		if s.Role != f.role {
			t.Errorf("statement of another role: %+v", s)
		}
	}
	if len(a.Statements) == 0 {
		t.Error("no statements attributed to the broker role")
	}
	req.Cursor = a.NextCursor
	next, err := ReadActivity(context.Background(), f.target, req)
	require.NoError(t, err)
	if len(next.Queries) != 1 || next.NextCursor != "" || next.Queries[0].RowCount == nil ||
		*next.Queries[0].RowCount != 2 {
		t.Errorf("second page = %+v (cursor %q), want the first read with 2 rows",
			next.Queries, next.NextCursor)
	}
}

const (
	sqlPeople = "SELECT id, ssn FROM people ORDER BY id"
	sqlUpper  = "SELECT upper(name) FROM people"
	sqlCast   = "SELECT id FROM people WHERE ssn::int = 0"
	sqlSecret = "SELECT api_key FROM people"
	sqlSSN    = "SELECT ssn FROM people ORDER BY id"
)

var maskStatements = map[string]scripted{
	sqlPeople: selectRead(sqlPeople, 0, []string{"people"}, nil, []string{"id", "ssn"},
		[]string{"id"}),
	sqlUpper: selectRead(sqlUpper, 0, []string{"people"}, []string{"upper"}, nil,
		[]string{"name"}),
	sqlCast: selectRead(sqlCast, 0, []string{"people"}, nil, []string{"id"},
		[]string{"ssn"}),
	sqlSecret: selectRead(sqlSecret, 0, []string{"people"}, nil, []string{"api_key"}, nil),
	sqlSSN: selectRead(sqlSSN, 0, []string{"people"}, nil, []string{"ssn"},
		[]string{"id"}),
}

// Column classes on the scripted path (tables only: a view's definition
// is read by the parser, so views stay with TestQueryMasksInDevAndDeniesInProd
// under cgo): pii is masked in dev and refused in prod, a masked column
// outside the output is refused, and a secret is refused everywhere.
func TestPathColumnClasses(t *testing.T) {
	f := newFixture(t, envbind.EnvDev)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	b, _ := f.pathBroker(t)
	useScriptedParser(b, maskStatements)
	res, err := f.ask(b, sqlPeople, 0)
	require.NoError(t, err)
	if res.Status != StatusOK || cell(t, res, 0, 0) != "1" || cell(t, res, 0, 1) != MaskedValue ||
		len(res.Masked) != 1 || res.Masked[0] != "ssn" || res.Columns[1].Class != "pii" {
		t.Errorf("dev pii = %+v, want ssn masked", res)
	}
	if res, _ = f.ask(b, sqlUpper, 0); res.Status != StatusOK ||
		cell(t, res, 0, 0) != MaskedValue {
		t.Errorf("expression over a pii relation = %+v, want masked", res)
	}
	// With a secret column the relation's expressions are refused too, so
	// the secret is classified only after the expression check.
	f.classifyColumn(t, "people", "api_key", classify.ClassSecret)
	classification := string(decide.ReasonClassification)
	for _, sql := range []string{sqlCast, sqlSecret} {
		if res, _ = f.ask(b, sql, 0); res.Verdict != VerdictBlocked ||
			res.ReasonCode != classification {
			t.Errorf("%q in dev = %+v, want blocked %s", sql, res, classification)
		}
	}
	prod := f.decider.verdict
	prod.Env = envbind.EnvProd
	f.setVerdict(prod)
	res, err = f.ask(b, sqlPeople, 0)
	require.NoError(t, err)
	if res.Verdict != VerdictBlocked || res.ReasonCode != classification ||
		!strings.Contains(res.Detail, "ssn") || len(res.Rows) != 0 {
		t.Errorf("prod pii = %+v, want blocked naming ssn", res)
	}
	audit := f.auditRows(t)
	if len(audit) != 5 || len(audit[3].classes) != 1 || audit[3].classes[0] != "secret" {
		t.Errorf("audit = %+v, want 5 rows, the secret class recorded", audit)
	}
}

// An unmask entry returns a pii value in prod on the scripted path.
func TestPathUnmaskEntryReturnsPII(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	u, err := New(f.cfg, Deps{Targets: fakeTargets{"db": f.target}, Logins: f.logins,
		Decider: f.decider, Classes: StoreClasses{}, Audit: &memAudit{},
		Unmasked: func(pid, dbid string, col classify.Column) bool {
			return pid == f.p.ID && dbid == f.target.DatabaseID && col.Name == "ssn"
		}})
	require.NoError(t, err)
	t.Cleanup(u.Close)
	useScriptedParser(u, maskStatements)
	res, err := f.ask(u, sqlSSN, 0)
	if err != nil || res.Status != StatusOK || cell(t, res, 0, 0) != "123-45-6789" ||
		len(res.Masked) != 0 {
		t.Errorf("unmasked pii in prod = %+v %v, want the value", res, err)
	}
}
