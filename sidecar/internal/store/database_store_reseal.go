package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// Reseal outcomes for one stored password.
const (
	resealCurrent    = "current"    // sage-enc:v1 under the active key
	resealSealed     = "sealed"     // an AAD-less value sealed now
	resealRotated    = "rotated"    // re-sealed from a previous key
	resealUnreadable = "unreadable" // no configured key opens it
)

type storedPassword struct {
	id  int
	enc []byte
}

// ResealDatabasePasswords seals every legacy password (raw crypto.Encrypt
// output, under the current, a pre-v0.9 or a previous key) and re-seals
// sage-enc:v1 values from a previous key to the active key (security-08).
// Each write is a compare-and-swap on the old value, so a password
// updated concurrently is never overwritten. Without a key it does
// nothing: passwords were never stored without one.
func (s *DatabaseStore) ResealDatabasePasswords(ctx context.Context) (SecretSealReport, error) {
	if s.keyring == nil {
		return SecretSealReport{}, nil
	}
	rows, err := s.passwordRows(ctx)
	if err != nil {
		return SecretSealReport{}, err
	}
	return s.resealRows(ctx, rows)
}

func (s *DatabaseStore) passwordRows(ctx context.Context) ([]storedPassword, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT id, password_enc
		 FROM sage.databases ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing stored database passwords: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedPassword, error) {
		var p storedPassword
		err := r.Scan(&p.id, &p.enc)
		return p, err
	})
}

func (s *DatabaseStore) resealRows(
	ctx context.Context, rows []storedPassword,
) (SecretSealReport, error) {
	var report SecretSealReport
	for _, row := range rows {
		next, kind := s.resealPassword(row.id, row.enc)
		if kind == resealUnreadable {
			s.logWrongKeyOnce(row.id)
			report.Unreadable++
		}
		if kind != resealSealed && kind != resealRotated {
			continue
		}
		swapped, err := s.replaceCiphertext(ctx, row.id, row.enc, next)
		if err != nil {
			return report, err
		}
		if swapped && kind == resealSealed {
			report.Sealed++
		} else if swapped {
			report.Rotated++
		}
	}
	return report, nil
}

// resealPassword decides what to do with one stored password and returns
// the value to write for resealSealed and resealRotated.
func (s *DatabaseStore) resealPassword(id int, enc []byte) ([]byte, string) {
	aad := dbPasswordAAD(id)
	kind := resealSealed
	var plain string
	if crypto.IsSealed(string(enc)) {
		var kid string
		var err error
		plain, kid, err = s.keyring.Open(string(enc), aad)
		if err != nil {
			return nil, resealUnreadable
		}
		if kid == s.keyring.ActiveKeyID() {
			return nil, resealCurrent
		}
		kind = resealRotated
	} else {
		var err error
		if plain, _, err = s.openLegacy(enc); err != nil {
			return nil, resealUnreadable
		}
	}
	next, err := s.keyring.Seal(plain, aad)
	if err != nil {
		return nil, resealUnreadable
	}
	return []byte(next), kind
}
