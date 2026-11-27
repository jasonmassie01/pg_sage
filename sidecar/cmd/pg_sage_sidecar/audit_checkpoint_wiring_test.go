package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// The evidence-signing key signs the chains' checkpoints under its own key
// id; no key (unsigned packs) signs nothing.
func TestCheckpointSignerFromTheEvidenceKey(t *testing.T) {
	if s := checkpointSigner(evidenceKey{}); s != nil {
		t.Fatalf("no evidence key gave signer %+v", s)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := checkpointSigner(evidenceKey{key: key, source: evidence.KeySourceGenerated})
	if s == nil || s.ID != evidence.KeyID(pub) || !s.Trusted().Public.Equal(pub) {
		t.Fatalf("signer = %+v, want key id %s", s, evidence.KeyID(pub))
	}
}

// Verification sources carry the signer and the prune switch; the switch
// is off by default and without a config.
func TestAuditSourcesCarryTheSignerAndPruneSwitch(t *testing.T) {
	prevCfg, prevSigner := cfg, auditSigner.Load()
	t.Cleanup(func() { cfg = prevCfg; auditSigner.Store(prevSigner) })
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := checkpointSigner(evidenceKey{key: key})
	auditSigner.Store(signer)
	cfg = config.DefaultConfig()
	sources := auditSources(&pgxpool.Pool{}, nil)
	got := sources()
	if len(got) != 1 || got[0].Signer != signer || got[0].PruneBelowSignedCheckpoint {
		t.Fatalf("default config: %+v, want the signer and pruning off", got)
	}
	cfg.Audit.PruneBelowSignedCheckpoint = true
	if got := sources(); !got[0].PruneBelowSignedCheckpoint {
		t.Fatal("prune switch on, sources do not carry it")
	}
	cfg = nil
	auditSigner.Store(nil)
	if got := sources(); got[0].PruneBelowSignedCheckpoint || got[0].Signer != nil {
		t.Fatalf("no config and no key: %+v", got[0])
	}
}

// The audit routes carry the prune switch too, so /audit/verify starts a
// pruned chain at its floor only when pruning is on (off by default and
// without a config).
func TestAuditAPIDepsCarryThePruneSwitch(t *testing.T) {
	if auditAPIDeps(nil, nil).PruneBelowSignedCheckpoint {
		t.Fatal("no config: the audit routes have pruning on")
	}
	c := config.DefaultConfig()
	if auditAPIDeps(c, nil).PruneBelowSignedCheckpoint {
		t.Fatal("default config: the audit routes have pruning on")
	}
	c.Audit.PruneBelowSignedCheckpoint = true
	if !auditAPIDeps(c, nil).PruneBelowSignedCheckpoint {
		t.Fatal("prune switch on: the audit routes do not carry it")
	}
}
