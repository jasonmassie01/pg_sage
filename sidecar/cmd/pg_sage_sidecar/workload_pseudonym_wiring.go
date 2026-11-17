package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// workloadPseudonymKey resolves the install's workload pack pseudonym key
// (evid-02c) from the control database, sealed under its encryption
// keyring and created on first use. Without an encryption key, or when the
// stored key does not open, it is nil and the model tier's
// hash_identifiers is refused; the key is never replaced or derived.
func workloadPseudonymKey(controlPool *pgxpool.Pool) []byte {
	if controlPool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), evidenceKeyTimeout)
	defer cancel()
	keys, err := notificationKeys(ctx, controlPool)
	if err != nil {
		logError("workpack", "workload pack identifier hashing is unavailable: the "+
			"encryption key of the control database is unavailable: %v", err)
		return nil
	}
	if keys == nil || keys.kr == nil {
		logInfo("workpack", "workload pack identifier hashing needs encryption_key")
		return nil
	}
	key, err := workpack.LoadOrCreatePseudonymKey(ctx, controlPool, keys.kr)
	if err != nil {
		logError("workpack", "workload pack identifier hashing is unavailable: %v", err)
		return nil
	}
	return key
}
