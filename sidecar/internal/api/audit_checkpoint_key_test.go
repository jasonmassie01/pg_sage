package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// No concurrent access tests here: checkpointKey reads immutable deps.

// GET /api/v1/audit/verify checks signed checkpoint links under the
// evidence key, by the id the evidence key route serves; without a key it
// checks none (and a pruned chain then fails, in auditchain).
func TestAuditVerifyCheckpointKey(t *testing.T) {
	if k := (AuditDeps{}).checkpointKey(); k != nil {
		t.Fatalf("no evidence key gave %+v", k)
	}
	if k := (AuditDeps{EvidenceKey: ed25519.PrivateKey{1, 2}}).checkpointKey(); k != nil {
		t.Fatalf("a malformed evidence key gave %+v", k)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := AuditDeps{EvidenceKey: key}.checkpointKey()
	if k == nil || k.ID != evidence.KeyID(pub) || !k.Public.Equal(pub) {
		t.Fatalf("checkpoint key = %+v, want id %s", k, evidence.KeyID(pub))
	}
}
