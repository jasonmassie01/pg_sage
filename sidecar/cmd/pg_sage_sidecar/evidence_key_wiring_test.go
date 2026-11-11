package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// audit-10 precedence: signing_key_env, then the stored key, then none.

// No concurrent access tests: resolution runs once at wiring; concurrent
// creators are covered in internal/evidence.

func testEvidenceKeyring(t *testing.T, fill byte) *crypto.Keyring {
	t.Helper()
	kr, err := crypto.NewKeyring(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return kr
}

func storedEvidenceRow(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	var keyID, sealed string
	if err := pool.QueryRow(context.Background(), `SELECT key_id, sealed
		FROM sage.evidence_signing_key WHERE id = 1`).Scan(&keyID, &sealed); err != nil {
		t.Fatalf("read the stored key: %v", err)
	}
	return keyID, sealed
}

// Boundary: an env key and a stored key are both present; the env key
// signs and the stored row is untouched.
func TestResolveEvidenceKey_EnvKeyWinsOverStored(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_envwins")
	ctx := context.Background()
	kr := testEvidenceKeyring(t, 2)
	stored := resolveEvidenceKey(ctx, testConfig(), pool, kr)
	if stored.source != evidence.KeySourceGenerated {
		t.Fatalf("setup: source %q", stored.source)
	}
	beforeID, beforeSealed := storedEvidenceRow(t, pool)
	_, envKey, _ := ed25519.GenerateKey(rand.Reader)
	pemText, _ := evidence.MarshalSigningKey(envKey)
	c := testConfig()
	c.Audit.Evidence.SigningKeyEnv = "SAGE_TEST_AUDIT10_KEY"
	t.Setenv("SAGE_TEST_AUDIT10_KEY", pemText)
	got := resolveEvidenceKey(ctx, c, pool, kr)
	if !got.key.Equal(envKey) || got.source != evidence.KeySourceEnv {
		t.Fatalf("env key: equal %v source %q", got.key.Equal(envKey), got.source)
	}
	if id, sealed := storedEvidenceRow(t, pool); id != beforeID || sealed != beforeSealed {
		t.Fatal("the stored key changed while the env key was in use")
	}
}

// Boundary: a configured env key that does not load leaves packs unsigned;
// it never falls back to the stored key, which the operator did not pick.
func TestResolveEvidenceKey_BrokenEnvKeyDoesNotFallBack(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_envbroken")
	ctx := context.Background()
	kr := testEvidenceKeyring(t, 3)
	if got := resolveEvidenceKey(ctx, testConfig(), pool, kr); got.key == nil {
		t.Fatal("setup: no stored key")
	}
	c := testConfig()
	c.Audit.Evidence.SigningKeyEnv = "SAGE_TEST_AUDIT10_BROKEN"
	t.Setenv("SAGE_TEST_AUDIT10_BROKEN", "not a key")
	var got evidenceKey
	logs := captureStderr(t, func() { got = resolveEvidenceKey(ctx, c, pool, kr) })
	if got.key != nil || got.source != "" {
		t.Fatalf("broken env key: key %v source %q, want unsigned", got.key != nil, got.source)
	}
	if !strings.Contains(logs, "[ERROR] [audit]") ||
		!strings.Contains(logs, "SAGE_TEST_AUDIT10_BROKEN") {
		t.Fatalf("no ERROR naming the variable: %q", logs)
	}
}

// Error propagation: a corrupt stored key logs ERROR, packs stay
// unsigned, and the row is not overwritten with a new key.
func TestResolveEvidenceKey_CorruptStoredKeyLogsErrorAndStaysUnsigned(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_corrupt")
	ctx := context.Background()
	kr := testEvidenceKeyring(t, 4)
	if got := resolveEvidenceKey(ctx, testConfig(), pool, kr); got.key == nil {
		t.Fatal("setup: no stored key")
	}
	corrupt := "sage-enc:v1:" + kr.ActiveKeyID() + ":AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := pool.Exec(ctx, `UPDATE sage.evidence_signing_key SET sealed = $1`,
		corrupt); err != nil {
		t.Fatal(err)
	}
	beforeID, _ := storedEvidenceRow(t, pool)
	var got evidenceKey
	logs := captureStderr(t, func() { got = resolveEvidenceKey(ctx, testConfig(), pool, kr) })
	if got.key != nil || got.source != "" {
		t.Fatalf("corrupt stored key: key %v source %q, want unsigned", got.key != nil,
			got.source)
	}
	if !strings.Contains(logs, "[ERROR] [audit]") || !strings.Contains(logs, "not be signed") {
		t.Fatalf("no ERROR for the unreadable key: %q", logs)
	}
	if id, sealed := storedEvidenceRow(t, pool); id != beforeID || sealed != corrupt {
		t.Fatalf("the corrupt row was overwritten: %s %s", id, sealed)
	}
}

// Nil/empty: no control pool means no stored key; packs stay unsigned
// without touching a database.
func TestResolveEvidenceKey_NoPoolIsUnsigned(t *testing.T) {
	got := resolveEvidenceKey(context.Background(), testConfig(), nil,
		testEvidenceKeyring(t, 5))
	if got.key != nil || got.source != "" {
		t.Fatalf("no pool: key %v source %q", got.key != nil, got.source)
	}
	if got := resolveEvidenceKey(context.Background(), nil, nil, nil); got.key != nil {
		t.Fatal("nil config produced a key")
	}
}
