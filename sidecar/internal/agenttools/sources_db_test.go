package agenttools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: QuerySources samples pg_stat_activity and
// keeps no state between calls. The sessions it samples are concurrent by
// construction (startSession runs on its own connection).

const cartSQL = "/*route='%2Fcart',controller='cart'*/ SELECT pg_sleep(5)"

// startSession runs sql on a new connection to dsn with application_name
// app and waits until pg_stat_activity shows it active. The statement is
// cancelled and the connection closed when the test ends.
func startSession(t *testing.T, f *fixture, dsn, app, sql string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["application_name"] = app
	conn, err := pgx.ConnectConfig(f.ctx, cfg)
	require.NoError(t, err)
	pid := conn.PgConn().PID()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Cancelled at cleanup; its "canceling statement" error is expected.
		_, _ = conn.Exec(context.Background(), sql)
	}()
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "SELECT pg_cancel_backend($1)", int64(pid))
		<-done
		_ = conn.Close(context.Background())
	})
	waitActive(t, f, int64(pid))
}

func waitActive(t *testing.T, f *fixture, pid int64) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var active bool
		f.scalar(&active, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid = $1 AND state = 'active')`, pid)
		if active {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %d never became active", pid)
}

func findSource(qs []QuerySource, app, route string) *QuerySource {
	for i := range qs {
		if qs[i].ApplicationNames[app] >= 1 && qs[i].Tags["route"] == route {
			return &qs[i]
		}
	}
	return nil
}

func TestQuerySourcesAttributesActiveTaggedSession(t *testing.T) {
	f := newFixture(t)
	startSession(t, f, f.dsn, "checkout-svc", cartSQL)
	tools := New(f.pool, Options{})
	start := time.Now()
	res, err := tools.QuerySources(f.ctx, SourcesRequest{SampleSeconds: 1})
	elapsed := time.Since(start)
	require.NoError(t, err)
	got := findSource(res.Queries, "checkout-svc", "/cart")
	require.NotNil(t, got, "tagged session not attributed: %+v", res.Queries)
	require.Equal(t, "cart", got.Tags["controller"])
	require.GreaterOrEqual(t, got.Samples, 1)
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond, "SampleSeconds 1 samples ~1s")
	require.Less(t, elapsed, 4*time.Second)
	require.NotEmpty(t, res.Method)
	require.NotEmpty(t, res.Note)
	explained := strings.ToLower(res.Method + " " + res.Note)
	for _, word := range []string{"pg_stat_activity", "sqlcommenter", "pg_stat_statements"} {
		require.Contains(t, explained, word, "method/note must explain the sources used")
	}
}

func TestQuerySourcesSingleSnapshot(t *testing.T) {
	f := newFixture(t)
	startSession(t, f, f.dsn, "checkout-svc", cartSQL)
	start := time.Now()
	res, err := New(f.pool, Options{}).QuerySources(f.ctx, SourcesRequest{})
	require.NoError(t, err, "SampleSeconds 0 is one snapshot")
	require.Less(t, time.Since(start), 900*time.Millisecond)
	got := findSource(res.Queries, "checkout-svc", "/cart")
	require.NotNil(t, got)
	require.Equal(t, 1, got.ApplicationNames["checkout-svc"])
}

func TestQuerySourcesRejectsSampleSecondsOutOfRange(t *testing.T) {
	f := newFixture(t)
	tools := New(f.pool, Options{})
	for _, s := range []int{6, -1, 3600} {
		start := time.Now()
		_, err := tools.QuerySources(f.ctx, SourcesRequest{SampleSeconds: s})
		require.ErrorIs(t, err, ErrInvalid, "SampleSeconds %d", s)
		require.Less(t, time.Since(start), 500*time.Millisecond, "rejected before sampling")
	}
}

func TestQuerySourcesQueryIDFilter(t *testing.T) {
	f := newFixture(t)
	startSession(t, f, f.dsn, "checkout-svc", cartSQL)
	tools := New(f.pool, Options{})
	res, err := tools.QuerySources(f.ctx, SourcesRequest{QueryID: 777000777000777})
	require.NoError(t, err)
	require.Len(t, res.Queries, 0, "unrelated query id matched: %+v", res.Queries)
	t.Run("matching id", func(t *testing.T) {
		var qid *int64
		f.scalar(&qid, `SELECT query_id FROM pg_stat_activity
			WHERE application_name = 'checkout-svc' AND state = 'active' LIMIT 1`)
		if qid == nil || *qid == 0 {
			t.Skip("pg_stat_activity.query_id not computed (compute_query_id off)")
		}
		res, err := tools.QuerySources(f.ctx, SourcesRequest{QueryID: QueryID(*qid)})
		require.NoError(t, err)
		require.Len(t, res.Queries, 1)
		require.Equal(t, QueryID(*qid), res.Queries[0].QueryID)
		require.Equal(t, "/cart", res.Queries[0].Tags["route"])
	})
}

// pg_sage's own statements, idle sessions and other databases' sessions
// are not workload of this database.
func TestQuerySourcesExcludesSelfIdleAndOtherDatabases(t *testing.T) {
	f := newFixture(t)
	startSession(t, f, f.dsn, "checkout-svc", cartSQL)
	startSession(t, f, f.dsn, "sage-self",
		"/* pg_sage */ SELECT pg_sleep(5) /*route='%2Fself'*/")
	other := extraDatabase(t, f.ctx, "sources_other")
	startSession(t, f, other.Config().ConnString(), "other-svc",
		"/*route='%2Fother'*/ SELECT pg_sleep(5)")
	idleCfg, err := pgx.ParseConfig(f.dsn)
	require.NoError(t, err)
	idleCfg.RuntimeParams["application_name"] = "idle-svc"
	idle, err := pgx.ConnectConfig(f.ctx, idleCfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idle.Close(context.Background()) })
	_, err = idle.Exec(f.ctx, "/*route='%2Fidle'*/ SELECT 1")
	require.NoError(t, err)
	res, err := New(f.pool, Options{}).QuerySources(f.ctx, SourcesRequest{SampleSeconds: 1})
	require.NoError(t, err)
	require.NotNil(t, findSource(res.Queries, "checkout-svc", "/cart"), "control missing")
	require.Nil(t, findSource(res.Queries, "sage-self", "/self"), "pg_sage statement listed")
	require.Nil(t, findSource(res.Queries, "other-svc", "/other"), "other database listed")
	require.Nil(t, findSource(res.Queries, "idle-svc", "/idle"), "idle session listed")
}
