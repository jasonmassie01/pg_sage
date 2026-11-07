package auth

import (
	"fmt"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

// equalizerHash caches a bcrypt hash of a fixed throwaway password at one
// cost. Authenticate compares against it when an account has no password
// hash, so that refusal costs as much as a wrong password.
type equalizerHash struct {
	cost int32
	hash []byte
}

var cachedEqualizer atomic.Pointer[equalizerHash]

// timingEqualizerHash returns the throwaway hash at the active cost,
// building it on first use and again whenever a test changes the cost.
// Concurrent first callers may each build one; any of them is valid.
func timingEqualizerHash() ([]byte, error) {
	cost := hashCost.Load()
	if e := cachedEqualizer.Load(); e != nil && e.cost == cost {
		return e.hash, nil
	}
	hash, err := bcrypt.GenerateFromPassword(
		[]byte("pg_sage timing equalizer"), int(cost))
	if err != nil {
		return nil, fmt.Errorf("building the login timing hash at cost %d: %w", cost, err)
	}
	cachedEqualizer.Store(&equalizerHash{cost: cost, hash: hash})
	return hash, nil
}

// equalizeRefusalTiming runs one bcrypt comparison of password against the
// throwaway hash and returns ErrInvalidCredentials, or the hash-building
// error (which also refuses the login).
func equalizeRefusalTiming(password string) error {
	hash, err := timingEqualizerHash()
	if err != nil {
		return err
	}
	_ = bcrypt.CompareHashAndPassword(hash, []byte(password))
	return ErrInvalidCredentials
}
