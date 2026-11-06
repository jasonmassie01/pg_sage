package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/siem"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func auditPost(mux http.Handler, path, body string, admin bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req = withUser(req, testAdminUser())
	} else {
		req = withUser(req, testViewerUser())
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// reanchorRecorder captures re-anchor requests and answers with res or err.
type reanchorRecorder struct {
	got []siem.ReanchorRequest
	res siem.ReanchorResult
	err error
}

func (r *reanchorRecorder) reanchor(_ context.Context,
	req siem.ReanchorRequest) (siem.ReanchorResult, error) {
	r.got = append(r.got, req)
	if r.err != nil {
		return siem.ReanchorResult{}, r.err
	}
	return r.res, nil
}

func reanchorEvents(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT detail::text FROM sage.auth_audit
		WHERE event = 'siem_reanchored' ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		require.NoError(t, rows.Scan(&d))
		out = append(out, d)
	}
	require.NoError(t, rows.Err())
	return out
}

// POST .../reanchor passes the sink from the path and the body (from_seq
// defaulting to 1) to the exporter, answers with the result and audits it.
func TestAuditSIEMReanchorRoute(t *testing.T) {
	pool := auditPool(t, "api_siem_reanchor")
	rec := &reanchorRecorder{res: siem.ReanchorResult{Sink: "soc", Source: "orders",
		Chain: "action_log", FromSeq: 1, PreviousSeq: 10, ForkTo: 10}}
	mux := auditMux(pool, AuditDeps{SIEMReanchor: rec.reanchor})
	w := auditPost(mux, "/api/v1/audit/siem/soc/reanchor",
		`{"source":"orders","chain":"action_log"}`, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, rec.got, 1)
	require.Equal(t, siem.ReanchorRequest{Sink: "soc", Source: "orders",
		Chain: "action_log", FromSeq: 1}, rec.got[0])
	var body siem.ReanchorResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, rec.res, body)
	events := reanchorEvents(t, pool)
	require.Len(t, events, 1)
	for _, want := range []string{`"sink": "soc"`, `"source": "orders"`,
		`"chain": "action_log"`, `"from_seq": 1`, `"previous_seq": 10`} {
		require.Contains(t, events[0], want)
	}
	w = auditPost(mux, "/api/v1/audit/siem/soc/reanchor",
		`{"source":"control","chain":"auth_audit","from_seq":5}`, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, int64(5), rec.got[1].FromSeq)
	require.Equal(t, "control", rec.got[1].Source)
}

// The route is admin-only, refuses bad input before reaching the
// exporter, maps the exporter's refusals distinguishably and audits only
// what it did.
func TestAuditSIEMReanchorRefusals(t *testing.T) {
	pool := auditPool(t, "api_siem_reanchor_refuse")
	rec := &reanchorRecorder{}
	mux := auditMux(pool, AuditDeps{SIEMReanchor: rec.reanchor})
	path := "/api/v1/audit/siem/soc/reanchor"
	ok := `{"source":"orders","chain":"action_log","from_seq":8}`
	require.Equal(t, http.StatusForbidden, auditPost(mux, path, ok, false).Code)
	off := auditMux(pool, AuditDeps{})
	require.Equal(t, http.StatusNotFound, auditPost(off, path, ok, true).Code)
	for name, body := range map[string]string{
		"malformed":         `{"source":`,
		"unknown field":     `{"source":"orders","chain":"action_log","seq":3}`,
		"no source":         `{"chain":"action_log"}`,
		"bad source":        `{"source":"a b","chain":"action_log"}`,
		"unknown chain":     `{"source":"orders","chain":"nope"}`,
		"negative from_seq": `{"source":"orders","chain":"action_log","from_seq":-1}`,
	} {
		w := auditPost(mux, path, body, true)
		require.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
	}
	require.Len(t, rec.got, 0, "bad input reached the exporter")
	for err, want := range map[error]int{
		siem.ErrUnknownSink: http.StatusNotFound, siem.ErrUnknownSource: http.StatusNotFound,
		siem.ErrNoCursor: http.StatusNotFound, siem.ErrNotLeader: http.StatusConflict,
		siem.ErrFenced: http.StatusConflict,
		fmt.Errorf("x: %w", siem.ErrInvalidReanchor): http.StatusBadRequest,
		errors.New("control database down"):          http.StatusInternalServerError,
	} {
		rec.err = err
		w := auditPost(mux, path, ok, true)
		require.Equal(t, want, w.Code, err.Error()+": "+w.Body.String())
	}
	require.Len(t, reanchorEvents(t, pool), 0, "a refused re-anchor was audited")
}

// chanSink records what it receives.
type chanSink struct {
	mu     sync.Mutex
	events []siem.Event
}

func (s *chanSink) Name() string { return "soc" }

func (s *chanSink) Send(_ context.Context, events []siem.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

func (s *chanSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// End to end on a real exporter: a restored-and-appended chain shows as
// regressed on GET /api/v1/audit/siem with nothing delivered, and the
// re-anchor route resumes it with the reused positions labelled a fork.
func TestAuditSIEMRegressionEndToEnd(t *testing.T) {
	pool := auditPool(t, "api_siem_regress")
	ctx := context.Background()
	sink := &chanSink{}
	e := siem.NewExporter(func() []siem.Source {
		return []siem.Source{{Name: "orders", DB: pool}}
	}, []siem.SinkConfig{{Sink: sink, Chains: []string{"action_log"}}},
		siem.NewCursorStore(pool), siem.Options{BatchSize: 50, Interval: time.Second},
		func() (siem.Fence, bool) { return siem.Fence{}, true }, nil)
	insert := func(n int) {
		_, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
			SELECT 'vacuum', 'VACUUM t' || g FROM generate_series(1, $1::int) g`, n)
		require.NoError(t, err)
	}
	insert(10)
	require.NoError(t, e.RunOnce(ctx))
	require.Equal(t, 10, sink.count())
	restoreActionLog(t, pool, 7)
	insert(3)
	require.NoError(t, e.RunOnce(ctx))
	require.Equal(t, 10, sink.count(), "a regressed chain delivered events")
	mux := auditMux(pool, AuditDeps{SIEMStatus: e.Status, SIEMReanchor: e.Reanchor})
	w := auditGet(mux, "/api/v1/audit/siem", true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"status":"regressed"`)
	require.Contains(t, w.Body.String(), `"chain":"action_log"`)
	w = auditPost(mux, "/api/v1/audit/siem/soc/reanchor",
		`{"source":"orders","chain":"action_log","from_seq":8}`, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, e.RunOnce(ctx))
	require.Equal(t, 13, sink.count())
	for _, ev := range sink.events[10:] {
		md := ev["metadata"].(map[string]any)
		require.Equal(t, []string{"fork"}, md["labels"])
	}
	w = auditGet(mux, "/api/v1/audit/siem", true)
	require.Contains(t, w.Body.String(), `"status":"ok"`)
}

// restoreActionLog drops the action_log links after keep with their rows,
// as a backup or PITR restore of the database does.
func restoreActionLog(t *testing.T, pool *pgxpool.Pool, keep int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "DELETE FROM sage.action_log WHERE chain_seq > $1", keep)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `DELETE FROM sage.audit_chain_link
		WHERE chain = 'action_log' AND seq > $1`, keep)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
}
