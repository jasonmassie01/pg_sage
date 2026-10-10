package agentguard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// killFixture is a role fixture whose pg_sage role may also signal
// backends (pg_signal_backend: the kill terminates agent sessions it does
// not inherit), with a kill switch over the fixture database.
type killFixture struct {
	*roleFixture
	sw       *Switch
	fallback *FallbackLog
	memory   *fakeMemory
	cfg      KillConfig
	targets  []KillTarget
}

func newKillFixture(t *testing.T) *killFixture {
	t.Helper()
	f := &killFixture{roleFixture: newRoleFixture(t), memory: &fakeMemory{}}
	f.grantSignal(t, true)
	f.grantRole(t, "pg_monitor") // pg_stat_replication details (unconfigured standbys)
	f.cfg = DefaultKillConfig()
	f.fallback = NewFallbackLog(filepath.Join(t.TempDir(), "kill-fallback.log"))
	f.targets = []KillTarget{{Name: f.db, Pool: f.admin, Executor: f.exec,
		ClusterKey: f.cluster.Key}}
	f.rebuild(t)
	t.Cleanup(func() { f.clearFlags() })
	return f
}

// rebuild makes the switch again after the fixture's targets, store or
// config changed.
func (f *killFixture) rebuild(t *testing.T) {
	t.Helper()
	sw, err := NewSwitch(KillDeps{Store: f.store, Keyring: f.manager.keyring,
		Targets: func(context.Context) ([]KillTarget, error) { return f.targets, nil },
		Memory:  []MemoryRevoker{f.memory}, Fallback: f.fallback, Config: f.cfg})
	require.NoError(t, err)
	f.sw = sw
}

func (f *killFixture) grantSignal(t *testing.T, grant bool) {
	t.Helper()
	var role string
	require.NoError(t, f.admin.QueryRow(context.Background(), "SELECT current_user").
		Scan(&role))
	stmt := "GRANT pg_signal_backend TO " + ident(role)
	if !grant {
		stmt = "REVOKE pg_signal_backend FROM " + ident(role)
	}
	_, err := f.super.Exec(context.Background(), stmt)
	require.NoError(t, err)
}

func (f *killFixture) grantRole(t *testing.T, role string) {
	t.Helper()
	var me string
	require.NoError(t, f.admin.QueryRow(context.Background(), "SELECT current_user").Scan(&me))
	_, err := f.super.Exec(context.Background(), "GRANT "+role+" TO "+ident(me))
	require.NoError(t, err)
}

// clearFlags lifts fleet and database flags a test left, so the next test
// starts unfrozen.
func (f *killFixture) clearFlags() {
	_, _ = f.super.Exec(context.Background(), `UPDATE sage.guard_freezes
		SET cleared_at = now(), cleared_by = 'test cleanup'
		WHERE cleared_at IS NULL AND scope IN ('fleet', 'database')`)
}

// ensured is a principal whose roles exist, and its broker password.
func (f *killFixture) ensured(t *testing.T) (Principal, string) {
	t.Helper()
	p := f.principal(t)
	_, err := f.manager.Ensure(context.Background(), f.request(p))
	require.NoError(t, err)
	_, password, err := f.store.BrokerCredential(context.Background(), f.manager.keyring,
		p.ID, f.cluster.Key)
	require.NoError(t, err)
	return p, password
}

// brokerStatement runs pg_sleep(seconds) as p's broker role and reports
// how it ended on done. started is closed once the backend is running.
type brokerStatement struct {
	started chan struct{}
	done    chan error
	elapsed time.Duration
}

func (f *killFixture) sleepAs(t *testing.T, watch rowQuerier, dsn, role, password string,
	seconds int) *brokerStatement {
	t.Helper()
	s := &brokerStatement{started: make(chan struct{}), done: make(chan error, 1)}
	conn, err := pgx.Connect(context.Background(), withUser(t, dsn, role, password))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	go func() {
		close(s.started)
		begin := time.Now()
		_, err := conn.Exec(context.Background(), fmt.Sprintf("SELECT pg_sleep(%d)", seconds))
		s.elapsed = time.Since(begin)
		s.done <- err
	}()
	<-s.started
	f.waitBackend(t, watch, role, 1)
	return s
}

// waitBackend waits until role has at least n active backends.
func (f *killFixture) waitBackend(t *testing.T, q rowQuerier, role string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countBackends(t, q, role) >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("role %s never had %d backends", role, n)
}

// rowQuerier is a pool or connection, on the primary or a standby.
type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func countBackends(t *testing.T, q rowQuerier, roles ...string) int {
	t.Helper()
	var n int
	require.NoError(t, q.QueryRow(context.Background(), `SELECT count(*)::int
		FROM pg_stat_activity WHERE usename = ANY($1)`, roles).Scan(&n))
	return n
}

// canLogin and connLimit read a role's attributes as superuser.
func (f *killFixture) attrs(t *testing.T, role string) (bool, int) {
	t.Helper()
	var login bool
	var limit int
	require.NoError(t, f.super.QueryRow(context.Background(), `SELECT rolcanlogin,
		rolconnlimit FROM pg_roles WHERE rolname = $1`, role).Scan(&login, &limit))
	return login, limit
}

// pendingApproval queues an approval for p on the fixture database.
func (f *killFixture) pendingApproval(t *testing.T, p Principal, status string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, f.super.QueryRow(context.Background(), `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, principal_id, proposed_via, proposed_by)
		VALUES ('SELECT 1', 'safe', $1, $2, 'agent', 'bot') RETURNING id`, status, p.ID).
		Scan(&id))
	t.Cleanup(func() {
		_, _ = f.super.Exec(context.Background(), "DELETE FROM sage.action_queue WHERE id = $1",
			id)
	})
	return id
}

func (f *killFixture) queueStatus(t *testing.T, id int64) string {
	t.Helper()
	var s string
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT status FROM sage.action_queue WHERE id = $1", id).Scan(&s))
	return s
}

// agentToken mints an agent token for p.
func (f *killFixture) agentToken(t *testing.T, p Principal) mcptoken.Token {
	t.Helper()
	tok, err := IssueToken(context.Background(), f.store, mcptoken.NewStore(f.super), p.ID,
		TokenRequest{Name: uniqName("tok"), Scopes: []string{"read"}, Databases: []string{"*"},
			ExpiresIn: 24 * time.Hour, CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	return tok
}

func (f *killFixture) tokenRevoked(t *testing.T, id string) bool {
	t.Helper()
	var revoked bool
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT revoked_at IS NOT NULL FROM sage.mcp_tokens WHERE id = $1", id).Scan(&revoked))
	return revoked
}

// actionCount counts p's action_log rows of a type recorded since t0. It
// filters by principal: the package's tests share the fixture database,
// and a time window alone also counted the previous test's kills.
func (f *killFixture) actionCount(t *testing.T, actionType string, p Principal,
	since time.Time) int {
	t.Helper()
	var n int
	require.NoError(t, f.super.QueryRow(context.Background(), `SELECT count(*)::int
		FROM sage.action_log WHERE action_type = $1 AND principal_id = $2
		AND executed_at >= $3`, actionType, p.ID, since).Scan(&n))
	return n
}

// waitDone waits for a statement to end, failing after limit.
func waitDone(t *testing.T, s *brokerStatement, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-time.After(limit):
		t.Fatalf("the brokered statement still runs after %s", limit)
		return nil
	}
}

// terminatedOrCancelled reports a statement ended by a signal: admin
// shutdown (57P01), query cancel (57014) or a closed connection.
func terminatedOrCancelled(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "57P01" || pg.Code == "57014"
	}
	return err != nil
}

// loginFails reports whether role can no longer log in with password.
func (f *killFixture) loginFails(t *testing.T, dsn, role, password string) bool {
	t.Helper()
	_, err := tryLogin(context.Background(), withUser(t, dsn, role, password))
	return err != nil
}
