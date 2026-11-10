package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

type verifyBody struct {
	OK      bool `json:"ok"`
	Reports []struct {
		Chain         string `json:"chain"`
		Mode          string `json:"mode"`
		FromSeq       int64  `json:"from_seq"`
		CheckpointSeq int64  `json:"checkpoint_seq"`
		Problems      []struct {
			Kind string `json:"kind"`
		} `json:"problems"`
	} `json:"reports"`
}

func getVerify(t *testing.T, mux http.Handler, path string) verifyBody {
	t.Helper()
	w := auditGet(mux, path, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body verifyBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Reports, 1)
	return body
}

// GET /api/v1/audit/verify verifies since the newest checkpoint by
// default, so an edit before it is not seen; full=true re-verifies the
// whole chain and finds it. Without a checkpoint the default is full.
func TestAuditVerifyDefaultsToSinceCheckpoint(t *testing.T) {
	pool := auditPool(t, "api_audit_verify_cp")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM a')`)
	require.NoError(t, err)
	mux := auditMux(pool, AuditDeps{})
	const path = "/api/v1/audit/verify?database=orders&chain=action_log"
	body := getVerify(t, mux, path)
	require.True(t, body.OK)
	require.Equal(t, auditchain.ModeFull, body.Reports[0].Mode)

	rep, err := auditchain.Verify(ctx, pool, auditchain.ActionLog, auditchain.Window{})
	require.NoError(t, err)
	require.NoError(t, auditchain.StoreCheckpoint(ctx, pool, rep))
	_, err = pool.Exec(ctx, "UPDATE sage.action_log SET sql_executed = 'x'")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM b')`)
	require.NoError(t, err)

	body = getVerify(t, mux, path)
	require.True(t, body.OK, "since-checkpoint must not re-read rows before it")
	require.Equal(t, auditchain.ModeIncremental, body.Reports[0].Mode)
	require.Equal(t, rep.HeadSeq, body.Reports[0].CheckpointSeq)
	require.Equal(t, rep.HeadSeq+1, body.Reports[0].FromSeq)

	body = getVerify(t, mux, path+"&full=true")
	require.False(t, body.OK)
	require.Equal(t, auditchain.ModeFull, body.Reports[0].Mode)
	require.Equal(t, "row_edited", body.Reports[0].Problems[0].Kind)
}

// full is a boolean and excludes an explicit window.
func TestAuditVerifyFullRefusals(t *testing.T) {
	pool := auditPool(t, "api_audit_verify_full")
	mux := auditMux(pool, AuditDeps{})
	for _, path := range []string{
		"/api/v1/audit/verify?database=orders&full=maybe",
		"/api/v1/audit/verify?database=orders&full=true&from_seq=2",
		"/api/v1/audit/verify?database=orders&full=true&to_seq=2",
	} {
		w := auditGet(mux, path, true)
		require.Equal(t, http.StatusBadRequest, w.Code, path+": "+w.Body.String())
	}
	body := getVerify(t, mux, "/api/v1/audit/verify?database=orders&chain=action_log"+
		"&full=false")
	require.True(t, body.OK)
}

// Error propagation: a verification that runs out of time answers 504
// naming the chain and the way out (windows), not a generic 500.
func TestAuditVerifyTimeoutIs504(t *testing.T) {
	pool := auditPool(t, "api_audit_verify_timeout")
	mux := auditMux(pool, AuditDeps{})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/audit/verify?database=orders&chain=action_log&full=true", nil)
	req = withUser(req.WithContext(ctx), testAdminUser())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	require.Equal(t, http.StatusGatewayTimeout, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "action_log")
	require.Contains(t, w.Body.String(), "from_seq/to_seq")
}
