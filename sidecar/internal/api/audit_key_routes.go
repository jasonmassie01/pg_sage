package api

import (
	"crypto/ed25519"
	"encoding/hex"
	"net/http"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// auditEvidenceKeyHandler serves GET /api/v1/audit/evidence/key (audit-10):
// the public half of the evidence-signing key, which an auditor passes to
// pg_sage_sidecar evidence verify --public-key or to openssl. A pack's own
// manifest names its signer too, but a forger would name theirs; this is
// the key to trust. 404 when packs are unsigned.
func auditEvidenceKeyHandler(d AuditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.EvidenceKey == nil {
			jsonError(w, "evidence packs are not signed: set encryption_key (the "+
				"sidecar then generates a signing key) or audit.evidence.signing_key_env",
				http.StatusNotFound)
			return
		}
		pub := d.EvidenceKey.Public().(ed25519.PublicKey)
		pemText, err := evidence.MarshalPublicKey(pub)
		if err != nil {
			internalError(w, r, "encode the evidence public key", err)
			return
		}
		source := d.EvidenceKeySource
		if source == "" {
			source = evidence.KeySourceEnv
		}
		jsonResponse(w, map[string]any{"key_id": evidence.KeyID(pub), "alg": "Ed25519",
			"public_key_pem": pemText, "public_key": hex.EncodeToString(pub),
			"source": source})
	}
}
