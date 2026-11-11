package main

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// evidenceKeyTimeout bounds reading or creating the stored key at wiring.
const evidenceKeyTimeout = 15 * time.Second

// evidenceKey is the key that signs evidence manifests and its source
// (evidence.KeySourceEnv or KeySourceGenerated); the zero value leaves
// packs hashed only.
type evidenceKey struct {
	key    ed25519.PrivateKey
	source string
}

// auditAPIDeps is the audit routes' dependencies: SIEM status and
// re-anchoring, and the evidence-signing key resolved from controlPool.
func auditAPIDeps(c *config.Config, controlPool *pgxpool.Pool) api.AuditDeps {
	signing := evidenceSigner(c, controlPool)
	d := api.AuditDeps{SIEMStatus: siemStatusFunc(c), SIEMReanchor: siemReanchorFunc(c),
		EvidenceKey: signing.key, EvidenceKeySource: signing.source,
		Health: auditHealthSnapshot}
	if c != nil {
		d.PruneBelowSignedCheckpoint = c.Audit.PruneBelowSignedCheckpoint
		d.VerifyIntervalHours = c.Audit.VerifyIntervalHours
	}
	return d
}

// evidenceSigner resolves the evidence-signing key for the API, reading
// the encryption keyring of the control database's secrets (audit-10).
func evidenceSigner(c *config.Config, controlPool *pgxpool.Pool) evidenceKey {
	ctx, cancel := context.WithTimeout(context.Background(), evidenceKeyTimeout)
	defer cancel()
	var kr *crypto.Keyring
	if c != nil && c.Audit.Evidence.SigningKeyEnv == "" && controlPool != nil {
		keys, err := notificationKeys(ctx, controlPool)
		if err != nil {
			logError("audit", "evidence packs will not be signed: the encryption key "+
				"of the control database is unavailable: %v", err)
			return evidenceKey{}
		}
		if keys != nil {
			kr = keys.kr
		}
	}
	return resolveEvidenceKey(ctx, c, controlPool, kr)
}

// resolveEvidenceKey applies the precedence: signing_key_env, then the key
// stored in pool sealed under kr (created on first use), then none. A
// configured env key that does not load leaves packs unsigned rather than
// signing with a key the operator did not choose; an unreadable stored key
// logs ERROR and is never replaced.
func resolveEvidenceKey(ctx context.Context, c *config.Config, pool *pgxpool.Pool,
	kr *crypto.Keyring) evidenceKey {
	if c == nil {
		return evidenceKey{}
	}
	if c.Audit.Evidence.SigningKeyEnv != "" {
		if key := evidenceSigningKey(c); key != nil {
			return evidenceKey{key: key, source: evidence.KeySourceEnv}
		}
		return evidenceKey{}
	}
	if pool == nil || kr == nil {
		logInfo("audit", "evidence packs are hashed, not signed: set encryption_key "+
			"or audit.evidence.signing_key_env to sign them")
		return evidenceKey{}
	}
	stored, err := evidence.LoadOrCreateKey(ctx, pool, kr)
	if err != nil {
		logError("audit", "evidence packs will not be signed: %v", err)
		return evidenceKey{}
	}
	if stored.ResealErr != nil {
		logWarn("audit", "evidence signing key %s: %v", stored.KeyID, stored.ResealErr)
	}
	if stored.Created {
		logInfo("audit", "generated evidence signing key %s (GET "+
			"/api/v1/audit/evidence/key serves its public key)", stored.KeyID)
	}
	return evidenceKey{key: stored.Key, source: evidence.KeySourceGenerated}
}
