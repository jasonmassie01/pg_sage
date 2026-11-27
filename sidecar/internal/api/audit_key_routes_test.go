package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// audit-10: GET /api/v1/audit/evidence/key serves the public half of the
// evidence-signing key, so an auditor can verify packs without trusting
// the key a pack names for itself.

// No concurrent access tests: the handler reads immutable deps only.

type evidenceKeyBody struct {
	KeyID        string `json:"key_id"`
	PublicKeyPEM string `json:"public_key_pem"`
	PublicKey    string `json:"public_key"`
	Source       string `json:"source"`
}

func readKeyBody(t *testing.T, raw []byte) evidenceKeyBody {
	t.Helper()
	var b evidenceKeyBody
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

// Happy path: the served key is the signing key, as PEM and as the
// manifest's hex, with its id and source.
func TestAuditEvidenceKeyRouteServesThePublicKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, source := range []string{evidence.KeySourceEnv, evidence.KeySourceGenerated} {
		mux := auditMux(nil, AuditDeps{EvidenceKey: priv, EvidenceKeySource: source})
		w := auditGet(mux, "/api/v1/audit/evidence/key", true)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		b := readKeyBody(t, w.Body.Bytes())
		require.Equal(t, evidence.KeyID(pub), b.KeyID)
		require.Equal(t, hex.EncodeToString(pub), b.PublicKey)
		require.Equal(t, source, b.Source)
		served, err := evidence.ParsePublicKey([]byte(b.PublicKeyPEM))
		require.NoError(t, err)
		require.True(t, served.Equal(pub), "served PEM is not the signing key")
		require.NotContains(t, w.Body.String(), "PRIVATE")
	}
}

// Boundary: a key wired without a source (a caller that predates the
// field) is reported as the configured one.
func TestAuditEvidenceKeyRouteDefaultsSourceToEnv(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := auditGet(auditMux(nil, AuditDeps{EvidenceKey: priv}), "/api/v1/audit/evidence/key",
		true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, evidence.KeySourceEnv, readKeyBody(t, w.Body.Bytes()).Source)
}

// Nil/empty: no key means 404 with a message that says how to get one;
// the route is admin-only either way.
func TestAuditEvidenceKeyRouteUnsignedAndAdminOnly(t *testing.T) {
	w := auditGet(auditMux(nil, AuditDeps{}), "/api/v1/audit/evidence/key", true)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "encryption_key")
	require.Contains(t, w.Body.String(), "signing_key_env")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	viewer := auditGet(auditMux(nil, AuditDeps{EvidenceKey: priv}),
		"/api/v1/audit/evidence/key", false)
	require.Equal(t, http.StatusForbidden, viewer.Code)
}

// CHECK-25 (integration): with an encryption key and no env key, the
// generated key signs the downloaded pack (X-Evidence-Signed: true) and
// the key route serves the key it verifies under.
func TestAuditEvidenceSignedByGeneratedKey(t *testing.T) {
	pool := auditPool(t, "api_audit_genkey")
	ctx := context.Background()
	kr, err := crypto.NewKeyring(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	stored, err := evidence.LoadOrCreateKey(ctx, pool, kr)
	require.NoError(t, err)
	var id int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed) VALUES ('vacuum', 'VACUUM t') RETURNING id`).Scan(&id))
	mux := auditMux(pool, AuditDeps{EvidenceKey: stored.Key,
		EvidenceKeySource: evidence.KeySourceGenerated})
	w := auditGet(mux, "/api/v1/audit/evidence?database=orders&action_id="+itoa64(id), true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "true", w.Header().Get("X-Evidence-Signed"))
	kw := auditGet(mux, "/api/v1/audit/evidence/key", true)
	require.Equal(t, http.StatusOK, kw.Code, kw.Body.String())
	b := readKeyBody(t, kw.Body.Bytes())
	require.Equal(t, stored.KeyID, b.KeyID)
	require.Equal(t, evidence.KeySourceGenerated, b.Source)
	pub, err := evidence.ParsePublicKey([]byte(b.PublicKeyPEM))
	require.NoError(t, err)
	m, err := evidence.Verify(bytes.NewReader(w.Body.Bytes()), pub)
	require.NoError(t, err)
	require.True(t, m.Signed)
	require.Equal(t, "", m.Integrity)
}

// Nil/empty (integration): without a key the pack is served unsigned and
// its manifest says its hashes guard against accidental corruption only.
func TestAuditEvidenceUnsignedCarriesTheIntegrityMarker(t *testing.T) {
	pool := auditPool(t, "api_audit_unsigned")
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed) VALUES ('vacuum', 'VACUUM t') RETURNING id`).Scan(&id))
	w := auditGet(auditMux(pool, AuditDeps{}),
		"/api/v1/audit/evidence?database=orders&action_id="+itoa64(id), true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "false", w.Header().Get("X-Evidence-Signed"))
	m, err := evidence.Verify(bytes.NewReader(w.Body.Bytes()), nil)
	require.NoError(t, err)
	require.False(t, m.Signed)
	require.Equal(t, evidence.IntegrityHashOnly, m.Integrity)
	require.Nil(t, m.Signer)
}
