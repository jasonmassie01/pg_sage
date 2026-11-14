package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// lazyPool is a pool that never connects (pgxpool dials on first use).
func lazyPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New(%q): %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

const lazyDSN = "postgres://sage:target-secret@db.invalid:5433/app?sslmode=disable"

func TestLoginRefusalMapsEveryCredentialFailure(t *testing.T) {
	cases := []struct {
		err    error
		reason string
	}{
		{fmt.Errorf("store: %w", agentguard.ErrNotFound), ReasonNoRole},
		{agentguard.ErrEncryptionKeyRequired, ReasonEncryptionKey},
		{&agentguard.DeniedError{Reason: agentguard.ReasonFrozen, Detail: "killed",
			Fix: "unfreeze"}, "agent_frozen"},
	}
	for _, c := range cases {
		res, failed, err := loginRefusal(c.err)
		if !failed || err != nil || res.Verdict != VerdictBlocked || res.ReasonCode != c.reason {
			t.Errorf("%v: %+v failed=%v err=%v, want blocked %s", c.err, res, failed, err,
				c.reason)
		}
	}
	if res, failed, err := loginRefusal(nil); failed || err != nil || res.Verdict != "" {
		t.Errorf("nil: %+v failed=%v err=%v, want no refusal", res, failed, err)
	}
	_, failed, err := loginRefusal(errors.New("control database down"))
	if !failed || !errors.Is(err, ErrUnavailable) ||
		!strings.Contains(err.Error(), "control database down") {
		t.Errorf("store down: failed=%v err=%v, want ErrUnavailable with the cause", failed, err)
	}
}

func TestPoolsEvictAndDropOnlyTheirKeys(t *testing.T) {
	p := newPools(DefaultConfig())
	for _, key := range []struct{ pid, db string }{{"a", "db1"}, {"a", "db2"}, {"b", "db1"}} {
		p.byKey[key.pid+"\x00"+key.db] = &loginPool{pool: lazyPool(t, lazyDSN),
			principalID: key.pid}
	}
	p.drop("b", "db2") // no such pool: nothing happens
	p.evict("a")
	if len(p.byKey) != 1 || p.byKey["b\x00db1"] == nil {
		t.Fatalf("after evicting a: %v, want only b on db1", p.byKey)
	}
	p.drop("b", "db1")
	if len(p.byKey) != 0 {
		t.Errorf("after dropping b on db1: %v, want none", p.byKey)
	}
	p.byKey["c\x00db1"] = &loginPool{pool: lazyPool(t, lazyDSN), principalID: "c"}
	p.evict("")
	if len(p.byKey) != 0 {
		t.Errorf("evict all left %v", p.byKey)
	}
}

// The broker pool keeps pg_sage's target but never its login.
func TestPoolConfigReplacesTheLogin(t *testing.T) {
	cfg := DefaultConfig()
	p := newPools(cfg)
	t0 := Target{Name: "db", Pool: lazyPool(t, lazyDSN)}
	pc, err := p.poolConfig(t0, "bq-bot\xff", "sage_agentb_x", "broker-pw")
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	c := pc.ConnConfig
	if c.User != "sage_agentb_x" || c.Password != "broker-pw" || c.Host != "db.invalid" ||
		c.Port != 5433 || c.Database != "app" {
		t.Errorf("conn = %s:%s@%s:%d/%s", c.User, c.Password, c.Host, c.Port, c.Database)
	}
	if c.RuntimeParams["application_name"] != "pg_sage agent:bq-bot" ||
		c.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Errorf("application_name %q, exec mode %v", c.RuntimeParams["application_name"],
			c.DefaultQueryExecMode)
	}
	if pc.MaxConns != int32(cfg.PoolMaxConns) || pc.MinConns != 0 ||
		pc.MaxConnIdleTime != cfg.PoolIdle {
		t.Errorf("pool bounds = %d/%d/%v", pc.MaxConns, pc.MinConns, pc.MaxConnIdleTime)
	}
	if t0.Pool.Config().ConnConfig.User != "sage" {
		t.Error("the target pool's own login was changed")
	}
	if _, err := p.poolConfig(Target{Name: "db"}, "bot", "r", "pw"); !errors.Is(err,
		ErrUnavailable) {
		t.Errorf("no target pool: error = %v, want ErrUnavailable", err)
	}
}

// A caller that gives up while the connection budget is full gets its
// context error and takes no slot.
func TestAcquireGivesUpWhenTheBudgetIsFull(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTotalConns, cfg.PoolMaxConns = 1, 1
	p := newPools(cfg)
	p.budget <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, release, err := p.acquire(ctx, Target{Name: "db"}, "a", "r", "pw")
	if !errors.Is(err, context.Canceled) || conn != nil || release != nil {
		t.Errorf("acquire = %v %v, want context.Canceled", conn, err)
	}
	if len(p.budget) != 1 {
		t.Errorf("budget holds %d slots, want 1", len(p.budget))
	}
	<-p.budget
	_, _, err = p.acquire(context.Background(), Target{Name: "db"}, "a", "r", "pw")
	if !errors.Is(err, ErrUnavailable) || len(p.budget) != 0 {
		t.Errorf("no target pool: %v with %d slots held, want ErrUnavailable and 0", err,
			len(p.budget))
	}
}

func unitCall(t *testing.T) (*call, *logSink) {
	t.Helper()
	logs := &logSink{}
	deps := boundDeps()
	deps.Log = logs.log
	b := newFakeBroker(t, deps)
	return &call{b: b, tool: "agent_query", t: Target{Name: "db"},
		id: agentguard.Identity{Principal: testPrincipal()}}, logs
}

// A rejected broker login names the role and drops the pool, so the next
// call logs in again (TestPathLoginFailureAfterRotation checks a real
// refusal for passwords in the error and the log).
func TestConnectFailureNamesRoleNotPassword(t *testing.T) {
	c, logs := unitCall(t)
	p := testPrincipal()
	c.b.pools.byKey[p.ID+"\x00db"] = &loginPool{pool: lazyPool(t, lazyDSN), principalID: p.ID}
	role := agentguard.BrokerRoleName(p.ID)
	cause := &pgconn.PgError{Code: "28P01", Message: "password authentication failed " +
		"for user \"" + role + "\""}
	res, err := c.connectFailure(p, err28(role, cause))
	if !errors.Is(err, ErrUnavailable) || res.Verdict != "" {
		t.Fatalf("connectFailure = %+v %v, want ErrUnavailable", res, err)
	}
	if !strings.Contains(err.Error(), role) || !strings.Contains(logs.text(), "28P01") {
		t.Errorf("error %q / log %q: want the role named and the cause logged", err,
			logs.text())
	}
	if len(c.b.pools.byKey) != 0 {
		t.Error("a rejected login kept the pool")
	}
	c.b.pools.byKey[p.ID+"\x00db"] = &loginPool{pool: lazyPool(t, lazyDSN), principalID: p.ID}
	_, err = c.connectFailure(p, errors.New("dial tcp: connection refused"))
	if !errors.Is(err, ErrUnavailable) || len(c.b.pools.byKey) != 1 {
		t.Errorf("network failure: %v, %d pools; want ErrUnavailable and the pool kept", err,
			len(c.b.pools.byKey))
	}
}

// err28 is a refused login as pgx reports it (pgx never quotes the
// password).
func err28(role string, pgErr *pgconn.PgError) error {
	return fmt.Errorf("failed to connect to `user=%s database=app`: %w", role, pgErr)
}

func TestDBFailureWithholdsValues(t *testing.T) {
	c, logs := unitCall(t)
	if _, err := c.dbFailure(context.Canceled); err != context.Canceled {
		t.Errorf("cancelled: %v, want context.Canceled as is", err)
	}
	if _, err := c.dbFailure(errors.New("connection reset")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("broken connection: %v, want ErrUnavailable", err)
	}
	res, err := c.dbFailure(&pgconn.PgError{Code: "22P02",
		Message: `invalid input syntax for type integer: "123-45-6789"`})
	if err != nil || res.Status != StatusFailed || res.SQLState != "22P02" ||
		strings.Contains(res.Message, "123-45-6789") ||
		strings.Contains(logs.text(), "123-45-6789") {
		t.Errorf("data exception = %+v %v / log %q: the value must be withheld", res, err,
			logs.text())
	}
	res, _ = c.dbFailure(&pgconn.PgError{Code: "42P01", Message: "relation x does not exist"})
	if !strings.Contains(res.Message, "does not exist or is not visible") ||
		!strings.Contains(logs.text(), "relation x does not exist") {
		t.Errorf("42P01 = %+v / log %q", res, logs.text())
	}
}

func TestRenderRowMasksAndCountsBytes(t *testing.T) {
	row, size := renderRow([][]byte{nil, []byte("abc"), []byte("secret")},
		[]columnAction{actPass, actPass, actMask})
	if row[0] != nil || *row[1] != "abc" || *row[2] != MaskedValue ||
		size != 3+len(MaskedValue) {
		t.Errorf("row = %v size %d", row, size)
	}
	if row, size := renderRow(nil, nil); len(row) != 0 || size != 0 {
		t.Errorf("empty row = %v size %d", row, size)
	}
	if actPass.String() != "pass" || actMask.String() != "mask" || actDeny.String() != "deny" {
		t.Error("column action names changed")
	}
}

// A statement's parameter count must match the given parameters before
// anything is decided or executed.
func TestRunRefusesAParameterMismatch(t *testing.T) {
	deps := boundDeps()
	b := newFakeBroker(t, deps)
	b.parse = scriptedParser(map[string]scripted{sqlByID: selectRead(sqlByID, 1,
		[]string{"items"}, nil, []string{"id"}, []string{"id"})})
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	_, err := b.Query(ctx, Request{Database: "db", SQL: sqlByID})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "1 parameters, 0 given") {
		t.Errorf("error = %v, want ErrInvalid naming the counts", err)
	}
	res, err := b.Query(ctx, Request{Database: "db", SQL: "SELECT 2"})
	if err != nil || res.ReasonCode != ReasonUnsupportedShape {
		t.Errorf("unscripted = %+v %v, want blocked %s", res, err, ReasonUnsupportedShape)
	}
	if n := deps.Logins.(*fakeLogins).calls; n != 0 || len(deps.Audit.(*memAudit).all()) != 1 {
		t.Errorf("%d logins, %d audit rows; want 0 and 1 (the refusal)", n,
			len(deps.Audit.(*memAudit).all()))
	}
}

func TestReadActivityRejectsBadRequests(t *testing.T) {
	ok := ActivityRequest{PrincipalID: "agp_x", Limit: 10}
	bad := []ActivityRequest{
		{PrincipalID: "agp_x", Limit: 0},
		{PrincipalID: "agp_x", Limit: MaxActivityLimit + 1},
		{Limit: 10},
		{PrincipalID: "agp_x", Limit: 10, Cursor: "abc"},
		{PrincipalID: "agp_x", Limit: 10, Cursor: "0"},
	}
	for _, req := range bad {
		if _, err := ReadActivity(context.Background(), Target{}, req); !errors.Is(err,
			ErrInvalid) {
			t.Errorf("%+v: error = %v, want ErrInvalid", req, err)
		}
	}
	if _, err := ReadActivity(context.Background(), Target{Name: "db"}, ok); !errors.Is(err,
		ErrUnavailable) {
		t.Errorf("no pool: error = %v, want ErrUnavailable", err)
	}
	if id, err := parseCursor("42"); err != nil || id != 42 {
		t.Errorf("cursor 42 = %d %v", id, err)
	}
}

// SQLAudit fails closed when it cannot write: no pool, or a pool that is
// gone.
func TestSQLAuditSinkFailure(t *testing.T) {
	rec := AuditRecord{PrincipalID: "agp_x", EnvelopeHash: "h", Verdict: VerdictExecute}
	err := SQLAudit{}.Record(context.Background(), Target{Name: "db"}, rec)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no pool for db") {
		t.Errorf("no pool: error = %v, want ErrUnavailable naming the database", err)
	}
	pool := lazyPool(t, lazyDSN)
	pool.Close()
	err = SQLAudit{}.Record(context.Background(), Target{Name: "db",
		DatabaseID: "00000000-0000-4000-8000-00000000b001", Pool: pool}, rec)
	if !errors.Is(err, ErrUnavailable) ||
		!strings.Contains(err.Error(), "recording the agent_query audit row") {
		t.Errorf("closed pool: error = %v, want ErrUnavailable from the insert", err)
	}
}
