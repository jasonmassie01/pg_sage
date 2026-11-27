package api

import (
	"bytes"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// evid-02c: a viewer reads a pack's model tier (exactly what would leave
// the network); only an admin downloads the signed export, which holds the
// local tier's query texts and is recorded in sage.auth_audit.
// No concurrency test here: ForModel's concurrent derivations are covered
// in workpack's model_tier_test.go.

func keyedPackMux(mgr *fleet.DatabaseManager, keys WorkloadPackKeys) *http.ServeMux {
	mux := http.NewServeMux()
	registerWorkloadPackRoutes(mux, mgr, config.DefaultConfig(), keys)
	return mux
}

func exportSigningKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize))
}

func rawDo(mux http.Handler, path string, user *auth.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(req, user))
	return w
}

func TestWorkloadPackTier_UnknownTierAndBadHashFlagAre400(t *testing.T) {
	mux := keyedPackMux(noPoolMgr(), WorkloadPackKeys{})
	sha := strings.Repeat("a", 64)
	for _, q := range []string{"?tier=remote", "?tier=MODEL", "?tier=model&hash_identifiers=yes",
		"?tier=local&hash_identifiers=true"} {
		code, body := packDo(t, mux, "GET", packURL("alpha", "/", sha, q), "", testViewerUser())
		if code != http.StatusBadRequest || body["code"] != "invalid_tier" {
			t.Errorf("%s: %d %v, want 400 invalid_tier", q, code, body)
		}
	}
}

func TestWorkloadPackExport_OperatorAndViewerAre403(t *testing.T) {
	mux := keyedPackMux(noPoolMgr(), WorkloadPackKeys{Signing: exportSigningKey()})
	path := packURL("alpha", "/", strings.Repeat("a", 64), "/export")
	for _, u := range []*auth.User{testOperatorUser(), testViewerUser()} {
		if w := rawDo(mux, path, u); w.Code != http.StatusForbidden {
			t.Errorf("%s export: %d, want 403", u.Role, w.Code)
		}
	}
}

func exportedRows(t *testing.T, pool *pgxpool.Pool, sha string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.auth_audit
		WHERE event = 'evidence_exported' AND detail->>'workload_pack' = $1`,
		sha).Scan(&n); err != nil {
		t.Fatalf("count evidence_exported: %v", err)
	}
	return n
}

func buildTestPack(t *testing.T, mux http.Handler) string {
	t.Helper()
	to := time.Now().UTC().Truncate(time.Hour)
	body := `{"from":"` + to.Add(-21*24*time.Hour).Format(time.RFC3339) + `","to":"` +
		to.Format(time.RFC3339) + `"}`
	code, built := packDo(t, mux, "POST", packURL("testdb"), body, testOperatorUser())
	sha, _ := built["content_sha256"].(string)
	if code != http.StatusCreated && code != http.StatusOK || len(sha) != 64 {
		t.Fatalf("build: %d %v", code, built)
	}
	return sha
}

func TestWorkloadPackTierDB_ViewerReadsTheModelTier(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	clearWorkloadPacks(t, pool)
	mux := keyedPackMux(phase2MgrWithPool(pool), WorkloadPackKeys{})
	sha := buildTestPack(t, mux)
	code, local := packDo(t, mux, "GET", packURL("testdb", "/", sha, "?tier=local"), "",
		testViewerUser())
	if code != http.StatusOK || local["manifest"] == nil || local["sections"] != nil {
		t.Fatalf("local tier: %d %v", code, local)
	}
	code, m := packDo(t, mux, "GET", packURL("testdb", "/", sha, "?tier=model"), "",
		testViewerUser())
	sections, _ := m["sections"].(map[string]any)
	if code != http.StatusOK || m["tier"] != "model" || sections["texts.json"] == nil ||
		sections["stats.json"] == nil || m["content_sha256"] == sha ||
		m["pseudonyms"] != nil || m["bytes"] == nil || m["tokens_estimate"] == nil {
		t.Fatalf("model tier: %d %v", code, m)
	}
	code, refused := packDo(t, mux, "GET", packURL("testdb", "/", sha,
		"?tier=model&hash_identifiers=true"), "", testViewerUser())
	if code != http.StatusConflict || refused["code"] != "no_pseudonym_key" ||
		!strings.Contains(refused["error"].(string), "keyring") {
		t.Fatalf("hashing without a key: %d %v, want 409 no_pseudonym_key", code, refused)
	}
	keyed := keyedPackMux(phase2MgrWithPool(pool),
		WorkloadPackKeys{Pseudonym: bytes.Repeat([]byte{2}, 32)})
	code, h := packDo(t, keyed, "GET", packURL("testdb", "/", sha,
		"?tier=model&hash_identifiers=true"), "", testViewerUser())
	if code != http.StatusOK || h["hash_identifiers"] != true || h["pseudonyms"] == nil ||
		h["content_sha256"] == m["content_sha256"] {
		t.Fatalf("hashed model tier: %d %v", code, h)
	}
}

func TestWorkloadPackExportDB_AdminDownloadsASignedAuditedExport(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	clearWorkloadPacks(t, pool)
	key := exportSigningKey()
	mux := keyedPackMux(phase2MgrWithPool(pool), WorkloadPackKeys{Signing: key, Control: pool})
	sha := buildTestPack(t, mux)
	before := exportedRows(t, pool, sha)
	path := packURL("testdb", "/", sha, "/export")
	if w := rawDo(mux, path, testOperatorUser()); w.Code != http.StatusForbidden ||
		exportedRows(t, pool, sha) != before {
		t.Fatalf("operator export: %d, rows %d -> %d", w.Code, before,
			exportedRows(t, pool, sha))
	}
	w := rawDo(mux, path, testAdminUser())
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/gzip" ||
		!strings.Contains(w.Header().Get("Content-Disposition"), sha[:12]) {
		t.Fatalf("admin export: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	m, err := evidence.Verify(bytes.NewReader(w.Body.Bytes()), key.Public().(ed25519.PublicKey))
	if err != nil || m.Format != "pg_sage-workload/1" || !m.Signed {
		t.Fatalf("verify the export: %v %+v", err, m)
	}
	if got := exportedRows(t, pool, sha); got != before+1 {
		t.Fatalf("evidence_exported rows %d -> %d, want one more", before, got)
	}
}

func TestWorkloadPackExportDB_NoSigningKeyIsRefused(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	clearWorkloadPacks(t, pool)
	mux := keyedPackMux(phase2MgrWithPool(pool), WorkloadPackKeys{Control: pool})
	sha := buildTestPack(t, mux)
	before := exportedRows(t, pool, sha)
	code, body := packDo(t, mux, "GET", packURL("testdb", "/", sha, "/export"), "",
		testAdminUser())
	if code != http.StatusConflict || body["code"] != "no_signing_key" ||
		!strings.Contains(body["error"].(string), "no evidence signing key") {
		t.Fatalf("unsigned export: %d %v, want 409 no_signing_key", code, body)
	}
	if exportedRows(t, pool, sha) != before {
		t.Fatal("a refused export was recorded as exported")
	}
}
