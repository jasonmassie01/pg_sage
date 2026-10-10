package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/siem"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func auditPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, schema.Bootstrap(ctx, pool))
	return pool
}

func auditMux(pool *pgxpool.Pool, deps AuditDeps) *http.ServeMux {
	if deps.Pool == nil {
		deps.Pool = func(name string) *pgxpool.Pool {
			if name == "orders" || name == "control" {
				return pool
			}
			return nil
		}
	}
	deps.Control = pool
	mux := http.NewServeMux()
	registerAuditRoutes(mux, deps)
	return mux
}

func auditGet(mux http.Handler, path string, admin bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if admin {
		req = withUser(req, testAdminUser())
	} else {
		req = withUser(req, testViewerUser())
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// GET /api/v1/audit/verify reports every chain of a database, and a
// tampered trail as not ok, naming the problem.
func TestAuditVerifyRoute(t *testing.T) {
	pool := auditPool(t, "api_audit_verify")
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed) VALUES ('vacuum', 'VACUUM t')`)
	require.NoError(t, err)
	mux := auditMux(pool, AuditDeps{})
	w := auditGet(mux, "/api/v1/audit/verify?database=orders", true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		OK      bool `json:"ok"`
		Reports []struct {
			Chain    string `json:"chain"`
			Links    int    `json:"links"`
			Problems []struct {
				Kind string `json:"kind"`
			} `json:"problems"`
		} `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.True(t, body.OK)
	require.GreaterOrEqual(t, len(body.Reports), 3)

	_, err = pool.Exec(context.Background(), "UPDATE sage.action_log SET sql_executed = 'x'")
	require.NoError(t, err)
	w = auditGet(mux, "/api/v1/audit/verify?database=orders&chain=action_log", true)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.False(t, body.OK)
	require.Len(t, body.Reports, 1)
	require.Equal(t, "row_edited", body.Reports[0].Problems[0].Kind)
}

// The audit routes are admin-only and refuse bad input distinguishably.
func TestAuditRoutesRefusals(t *testing.T) {
	pool := auditPool(t, "api_audit_refuse")
	mux := auditMux(pool, AuditDeps{})
	cases := map[string]int{
		"/api/v1/audit/verify?database=nope":                       http.StatusNotFound,
		"/api/v1/audit/verify?database=orders&chain=nope":          http.StatusBadRequest,
		"/api/v1/audit/verify?database=orders&from_seq=x":          http.StatusBadRequest,
		"/api/v1/audit/verify?database=orders&from_seq=9&to_seq=2": http.StatusBadRequest,
		"/api/v1/audit/evidence?database=orders":                   http.StatusBadRequest,
		"/api/v1/audit/evidence?database=orders&action_id=99999":   http.StatusNotFound,
		"/api/v1/audit/evidence?database=orders&from=yesterday":    http.StatusBadRequest,
		"/api/v1/audit/evidence?database=nope&action_id=1":         http.StatusNotFound,
	}
	for path, want := range cases {
		w := auditGet(mux, path, true)
		require.Equal(t, want, w.Code, path+": "+w.Body.String())
	}
	for _, path := range []string{"/api/v1/audit/verify?database=orders",
		"/api/v1/audit/evidence?database=orders&action_id=1", "/api/v1/audit/siem"} {
		require.Equal(t, http.StatusForbidden, auditGet(mux, path, false).Code, path)
	}
}

// GET /api/v1/audit/evidence downloads a pack that verifies, signed when a
// key is configured, and the export itself is audited.
func TestAuditEvidenceRoute(t *testing.T) {
	pool := auditPool(t, "api_audit_evidence")
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed) VALUES ('vacuum', 'VACUUM t') RETURNING id`).Scan(&id))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	mux := auditMux(pool, AuditDeps{EvidenceKey: priv})
	w := auditGet(mux, "/api/v1/audit/evidence?database=orders&action_id="+itoa64(id), true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "application/gzip", w.Header().Get("Content-Type"))
	require.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	require.Equal(t, "true", w.Header().Get("X-Evidence-Signed"))
	m, err := evidence.Verify(bytes.NewReader(w.Body.Bytes()), pub)
	require.NoError(t, err)
	require.Equal(t, id, m.Scope.ActionID)
	require.Equal(t, "orders", m.Scope.Database)

	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w = auditGet(mux, "/api/v1/audit/evidence?database=orders&from="+from+"&to="+to, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var exported int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.auth_audit WHERE event = 'evidence_exported'`).Scan(&exported))
	require.Equal(t, 2, exported)
}

// GET /api/v1/audit/siem reports the sinks' health, or that export is off.
func TestAuditSIEMStatusRoute(t *testing.T) {
	pool := auditPool(t, "api_audit_siem")
	off := auditGet(auditMux(pool, AuditDeps{}), "/api/v1/audit/siem", true)
	require.Equal(t, http.StatusOK, off.Code)
	require.JSONEq(t, `{"enabled":false,"sinks":[]}`, off.Body.String())
	on := auditMux(pool, AuditDeps{SIEMStatus: func() []siem.SinkStatus {
		return []siem.SinkStatus{{Name: "soc", Delivered: 12, ConsecutiveFailures: 2,
			LastError: "receiver answered HTTP 503"}}
	}})
	w := auditGet(on, "/api/v1/audit/siem", true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"enabled":true`)
	require.Contains(t, w.Body.String(), `"consecutive_failures":2`)
}

func itoa64(n int64) string { return jsonNumber(n) }

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
