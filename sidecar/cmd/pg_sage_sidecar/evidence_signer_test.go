package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// audit-10: evidenceSigner is what wireRouter calls. It reads the control
// database's encryption keyring the way notification secrets do, so an
// encryption_key alone is enough for packs to be signed.

// No concurrent access tests: wiring calls it once; concurrent creators
// are evidence.TestLoadOrCreateKey_ConcurrentCallersAgree.

func useSignerGlobals(t *testing.T, mode, encryptionKey string) *config.Config {
	t.Helper()
	oldCfg, oldMeta := cfg, globalMetaState
	t.Cleanup(func() {
		cfg, globalMetaState = oldCfg, oldMeta
		resetSecretsAtRest()
	})
	resetSecretsAtRest()
	globalMetaState = nil
	cfg = config.DefaultConfig()
	cfg.Mode, cfg.EncryptionKey = mode, encryptionKey
	return cfg
}

// Happy path and state transition: with an encryption key and no env key
// the sidecar signs with a generated key, and a restart (key caches
// forgotten) signs with the same key.
func TestEvidenceSigner_EncryptionKeyAloneSigns(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_signer")
	c := useSignerGlobals(t, "standalone", "audit10-passphrase")
	first := evidenceSigner(c, pool)
	if first.key == nil || first.source != evidence.KeySourceGenerated {
		t.Fatalf("encryption key set: key %v source %q, want a generated key",
			first.key != nil, first.source)
	}
	id, _ := storedEvidenceRow(t, pool)
	resetSecretsAtRest()
	again := evidenceSigner(c, pool)
	if !again.key.Equal(first.key) || again.source != evidence.KeySourceGenerated {
		t.Fatalf("restart: same key %v source %q", again.key.Equal(first.key), again.source)
	}
	if after, _ := storedEvidenceRow(t, pool); after != id {
		t.Fatalf("restart changed the stored key id %s -> %s", id, after)
	}
	deps := auditAPIDeps(c, pool)
	if !deps.EvidenceKey.Equal(first.key) ||
		deps.EvidenceKeySource != evidence.KeySourceGenerated {
		t.Fatalf("the audit routes get key %v source %q", deps.EvidenceKey != nil,
			deps.EvidenceKeySource)
	}
}

// Nil/empty: no encryption key leaves packs unsigned and stores nothing.
func TestEvidenceSigner_NoEncryptionKeyIsUnsigned(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_signer_nokey")
	c := useSignerGlobals(t, "standalone", "")
	if got := evidenceSigner(c, pool); got.key != nil || got.source != "" {
		t.Fatalf("no encryption key: key %v source %q", got.key != nil, got.source)
	}
	if got := evidenceSigner(c, nil); got.key != nil {
		t.Fatal("no control pool produced a key")
	}
	if got := evidenceSigner(nil, pool); got.key != nil {
		t.Fatal("nil config produced a key")
	}
	assertNoStoredEvidenceKey(t, pool)
}

// Boundary: meta mode keeps its keys on the meta database; a pool that is
// not the meta pool gets no key, and nothing is written to it.
func TestEvidenceSigner_MetaModeIgnoresANonMetaPool(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_signer_meta")
	c := useSignerGlobals(t, config.ModeMeta, "audit10-passphrase")
	if got := evidenceSigner(c, pool); got.key != nil {
		t.Fatalf("meta mode signed with a key stored in a non-meta pool (%q)", got.source)
	}
	assertNoStoredEvidenceKey(t, pool)
}

func assertNoStoredEvidenceKey(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.evidence_signing_key`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d stored evidence keys (%v), want 0", n, err)
	}
}
