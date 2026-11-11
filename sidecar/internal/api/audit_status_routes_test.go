package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/siem"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// audit-12: GET /api/v1/audit/status serves the last scheduled
// verification of every chain, SIEM delivery health and whether evidence
// packs are signed. It never verifies on demand (that is /audit/verify).

// No concurrent access tests: the handler reads the registry's snapshot,
// whose locking auditjob's tests cover.

type auditStatusBody struct {
	OK                  *bool                       `json:"ok"`
	VerifiedAt          *time.Time                  `json:"verified_at"`
	VerifyIntervalHours int                         `json:"verify_interval_hours"`
	Chains              []auditjob.ChainHealth      `json:"chains"`
	VerifyErrors        []auditjob.VerifyErrorCount `json:"verify_errors"`
	SIEM                struct {
		Enabled bool              `json:"enabled"`
		Sinks   []siem.SinkStatus `json:"sinks"`
	} `json:"siem"`
	Evidence struct {
		Signed bool   `json:"signed"`
		KeyID  string `json:"key_id"`
		Source string `json:"source"`
	} `json:"evidence"`
}

func readStatus(t *testing.T, raw []byte) auditStatusBody {
	t.Helper()
	var b auditStatusBody
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

// Nil/empty: before any verification the status says verified_at null
// (not epoch 0), lists no chains as an empty array, and its overall ok is
// unknown (null); export off and packs unsigned are reported as such.
func TestAuditStatusBeforeAnyVerification(t *testing.T) {
	for name, deps := range map[string]AuditDeps{
		"no registry":    {},
		"empty registry": {Health: auditjob.NewHealthRegistry().Snapshot},
	} {
		w := auditGet(auditMux(nil, deps), "/api/v1/audit/status", true)
		require.Equal(t, http.StatusOK, w.Code, name+": "+w.Body.String())
		raw := w.Body.String()
		for _, want := range []string{`"verified_at":null`, `"chains":[]`, `"ok":null`,
			`"verify_errors":[]`, `"sinks":[]`} {
			if !strings.Contains(raw, want) {
				t.Errorf("%s: body lacks %s: %s", name, want, raw)
			}
		}
		if strings.Contains(raw, "0001-01-01") || strings.Contains(raw, "1970-01-01") {
			t.Errorf("%s: body carries a zero time: %s", name, raw)
		}
		b := readStatus(t, w.Body.Bytes())
		require.False(t, b.SIEM.Enabled, name)
		require.False(t, b.Evidence.Signed, name)
		require.Equal(t, "", b.Evidence.KeyID, name)
	}
}

// Happy path: a red chain, its problems and the error counter come back
// as recorded, the overall ok is false, the sinks are those of the
// exporter and a signing key is named by its id.
func TestAuditStatusReportsRegistrySinksAndSigning(t *testing.T) {
	at := time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC)
	h := auditjob.NewHealthRegistry()
	h.Record("orders", []auditchain.Report{
		{Chain: "action_log", HeadSeq: 7, VerifiedAt: at,
			Problems: []auditchain.Problem{{Kind: auditchain.KindRowEdited, Seq: 2}}},
		{Chain: "auth_audit", HeadSeq: 1, VerifiedAt: at.Add(-time.Hour)},
	}, errors.New("config_audit: timeout"), at)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sinks := []siem.SinkStatus{{Name: "soc", Status: siem.StatusFailing,
		ConsecutiveFailures: 3, Lanes: []siem.LaneStatus{{Source: "orders",
			Chain: "action_log", Cursor: 2, Head: 7, Lag: 5}}}}
	deps := AuditDeps{Health: h.Snapshot, VerifyIntervalHours: 24, EvidenceKey: priv,
		EvidenceKeySource: evidence.KeySourceGenerated,
		SIEMStatus:        func() []siem.SinkStatus { return sinks }}
	w := auditGet(auditMux(nil, deps), "/api/v1/audit/status", true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	b := readStatus(t, w.Body.Bytes())
	require.True(t, b.OK != nil && !*b.OK, "overall ok must be false with a red chain")
	require.True(t, b.VerifiedAt != nil && b.VerifiedAt.Equal(at), "verified_at")
	require.Equal(t, 24, b.VerifyIntervalHours)
	require.Equal(t, 2, len(b.Chains))
	red := b.Chains[0]
	require.Equal(t, "action_log", red.Chain)
	require.False(t, red.OK, "action_log ok")
	require.Equal(t, 1, red.Problems[auditchain.KindRowEdited])
	require.True(t, b.Chains[1].OK, "auth_audit ok")
	require.Equal(t, 1, len(b.VerifyErrors))
	require.Equal(t, int64(1), b.VerifyErrors[0].Count)
	require.True(t, b.SIEM.Enabled, "siem enabled")
	require.Equal(t, int64(5), b.SIEM.Sinks[0].Lanes[0].Lag)
	require.True(t, b.Evidence.Signed, "signed")
	require.Equal(t, evidence.KeyID(pub), b.Evidence.KeyID)
	require.Equal(t, evidence.KeySourceGenerated, b.Evidence.Source)
}

// State transition: once every chain verifies clean the overall ok is
// true.
func TestAuditStatusAllGreen(t *testing.T) {
	h := auditjob.NewHealthRegistry()
	h.Record("orders", []auditchain.Report{{Chain: "action_log", VerifiedAt: time.Now()}},
		nil, time.Now())
	w := auditGet(auditMux(nil, AuditDeps{Health: h.Snapshot}), "/api/v1/audit/status", true)
	b := readStatus(t, w.Body.Bytes())
	require.True(t, b.OK != nil && *b.OK, "overall ok must be true: "+w.Body.String())
}

// Invalid input: the route is admin-only, like every audit route.
func TestAuditStatusIsAdminOnly(t *testing.T) {
	w := auditGet(auditMux(nil, AuditDeps{}), "/api/v1/audit/status", false)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
