package main

import (
	"errors"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// logStorePrepareFailure logs a store database that failed to prepare at
// startup. A wrong encryption_key is a WARN here: the store has already
// logged the one ERROR for that database id, with the recovery hint, so a
// fleet start logs one wrong-key ERROR per database (apiauth-11).
func logStorePrepareFailure(name string, err error) {
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		logWarn("meta-db", "db %q: prepare: %v", name, err)
		return
	}
	logError("meta-db", "db %q: prepare: %v", name, err)
}
