package agentguard

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G1-05 on a real streaming topology: SAGE_TEST_DATABASE_URL is the
// primary, SAGE_TEST_KILL_REPLICA_URL a standby configured as a replica
// (databases[].replicas) and SAGE_TEST_KILL_STANDBY_URL a standby pg_sage
// was not told about. Both standbys are pg_basebackup copies streaming
// from the primary, with cluster_name set (the walreceiver reports it as
// application_name). URLs are superuser DSNs; the test uses the fixture
// database on each.

const (
	envKillReplica     = "SAGE_TEST_KILL_REPLICA_URL"
	envKillStandby     = "SAGE_TEST_KILL_STANDBY_URL"
	envKillReplicaName = "SAGE_TEST_KILL_REPLICA_NAME"
	envKillStandbyName = "SAGE_TEST_KILL_STANDBY_NAME"
)

type topology struct {
	replicaDSN, standbyDSN   string
	replicaName, standbyName string
	replica, standby         *pgxpool.Pool // superuser on the fixture database
}

func newTopology(t *testing.T, f *killFixture) topology {
	t.Helper()
	rURL, sURL := os.Getenv(envKillReplica), os.Getenv(envKillStandby)
	if rURL == "" || sURL == "" {
		t.Skipf("G1-05 needs a streaming replica pair: set %s and %s (see "+
			"scripts/ci/kill-replica-topology.sh)", envKillReplica, envKillStandby)
	}
	top := topology{replicaDSN: withDatabase(t, rURL, f.db),
		standbyDSN:  withDatabase(t, sURL, f.db),
		replicaName: envOr(envKillReplicaName, "replica1"),
		standbyName: envOr(envKillStandbyName, "standby2")}
	ctx := context.Background()
	var err error
	top.replica, err = pgxpool.New(ctx, top.replicaDSN)
	require.NoError(t, err)
	t.Cleanup(top.replica.Close)
	top.standby, err = pgxpool.New(ctx, top.standbyDSN)
	require.NoError(t, err)
	t.Cleanup(top.standby.Close)
	return top
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func withDatabase(t *testing.T, dsn, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + db
	return u.String()
}

// replayed waits until a standby can log in as role.
func replayed(t *testing.T, dsn, role, password string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := tryLogin(context.Background(), withUser(t, dsn, role, password)); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("role %s never became usable on %s", role, dsn)
}

// eventuallyLoginFails waits for the NOLOGIN to replay on a standby.
func eventuallyLoginFails(t *testing.T, f *killFixture, dsn, role, password string,
	within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if f.loginFails(t, dsn, role, password) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func TestKill_G1_05_PrimaryAndReplicas(t *testing.T) {
	f := newKillFixture(t)
	top := newTopology(t, f)
	p, password := f.ensured(t)
	replayed(t, top.replicaDSN, p.BrokerRole(), password)
	replayed(t, top.standbyDSN, p.BrokerRole(), password)
	f.targets[0].Replicas = []Replica{{Name: top.replicaName, DSN: top.replicaDSN}}
	f.rebuild(t)
	pending := f.pendingApproval(t, p, "pending")
	onPrimary := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	onReplica := f.sleepAs(t, top.replica, top.replicaDSN, p.BrokerRole(), password, 30)

	begin := time.Now()
	rep, err := f.sw.Kill(context.Background(), KillRequest{Scope: KillScopeAll,
		Reason: "G1-05 drill", Actor: "admin@example.com"})
	require.NoError(t, err)
	require.Less(t, time.Since(begin), 10*time.Second)

	// Both statements are cancelled, within the bound.
	require.True(t, terminatedOrCancelled(waitDone(t, onPrimary, 10*time.Second)))
	require.True(t, terminatedOrCancelled(waitDone(t, onReplica, 10*time.Second)))
	require.Less(t, time.Since(begin), 10*time.Second)
	// No agent backend remains on either.
	roles := []string{p.BrokerRole(), p.LoginRole()}
	require.Equal(t, 0, countBackends(t, f.super, roles...))
	require.Equal(t, 0, countBackends(t, top.replica, roles...))
	// New logins fail on both, and on the unconfigured standby.
	require.True(t, f.loginFails(t, f.dsn, p.BrokerRole(), password))
	require.True(t, eventuallyLoginFails(t, f, top.replicaDSN, p.BrokerRole(), password,
		5*time.Second))
	require.True(t, eventuallyLoginFails(t, f, top.standbyDSN, p.BrokerRole(), password,
		5*time.Second))
	// Approvals are cancelled_kill.
	require.Equal(t, "cancelled_kill", f.queueStatus(t, pending))

	checkG105Report(t, top, rep, serverVersionNum(t, f.super))
}

// checkG105Report: the configured replica is verified; the unconfigured
// standby is named with its timeout bound, and only once.
func checkG105Report(t *testing.T, top topology, rep KillReport, version int) {
	t.Helper()
	require.True(t, rep.Verified)
	db := rep.Databases[0]
	require.True(t, db.Verified)
	var configured, unconfigured *ReplicaReport
	for i := range db.Replicas {
		r := &db.Replicas[i]
		switch {
		case r.Configured && r.Name == top.replicaName:
			configured = r
		case !r.Configured && r.ApplicationName == top.standbyName:
			unconfigured = r
		}
		require.False(t, !r.Configured && r.ApplicationName == top.replicaName,
			"the configured replica is not also reported as unconfigured")
	}
	require.NotNil(t, configured, "report: %+v", db.Replicas)
	require.True(t, configured.Verified)
	require.True(t, configured.LoginsBlocked)
	require.GreaterOrEqual(t, configured.BackendsTerminated, 1)
	require.Empty(t, configured.Error)
	require.NotNil(t, unconfigured, "the unconfigured standby is named: %+v", db.Replicas)
	require.NotNil(t, unconfigured.Bound)
	require.Equal(t, int64(30000), unconfigured.Bound.StatementTimeoutMS)
	require.Equal(t, int64(600000), unconfigured.Bound.IdleSessionTimeoutMS)
	wantTx := int64(0)
	if version >= 170000 {
		wantTx = 600000
	}
	require.Equal(t, wantTx, unconfigured.Bound.TransactionTimeoutMS)
	require.False(t, unconfigured.Verified)
}

// A configured replica that cannot be reached is reported with an error;
// the primary is still contained and the kill does not wait past its
// bound.
func TestKill_UnreachableReplicaReported(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	f.cfg.ReplicaConnectTimeout = time.Second
	f.targets[0].Replicas = []Replica{{Name: "gone",
		DSN: "postgres://postgres:postgres@127.0.0.1:1/postgres?sslmode=disable"}}
	f.rebuild(t)
	begin := time.Now()
	rep, err := f.sw.Kill(context.Background(), killPrincipal(p))
	require.NoError(t, err)
	require.Less(t, time.Since(begin), 10*time.Second)
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login)
	var gone *ReplicaReport
	for i := range rep.Databases[0].Replicas {
		if rep.Databases[0].Replicas[i].Name == "gone" {
			gone = &rep.Databases[0].Replicas[i]
		}
	}
	require.NotNil(t, gone)
	require.True(t, gone.Configured)
	require.False(t, gone.Verified)
	require.NotEmpty(t, gone.Error)
	require.False(t, rep.Databases[0].Verified)
	require.False(t, rep.Verified)
}
