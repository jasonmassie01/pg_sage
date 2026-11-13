package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// ResealActionPasswords re-seals every stored action password sealed under
// a previous key to the active key (security-09b). Action passwords were
// only ever written as sage-enc:v1, so there is no legacy form to seal; a
// value no configured key opens is counted unreadable and left as stored.
// Each write is a compare-and-swap on the old value, so a credential
// updated concurrently is never overwritten. Without a key it does nothing.
func (s *DatabaseStore) ResealActionPasswords(ctx context.Context) (SecretSealReport, error) {
	if s.keyring == nil {
		return SecretSealReport{}, nil
	}
	rows, err := s.actionPasswordRows(ctx)
	if err != nil {
		return SecretSealReport{}, err
	}
	var report SecretSealReport
	for _, row := range rows {
		next, kind := s.resealActionPassword(row.id, row.enc)
		switch kind {
		case resealUnreadable:
			s.logActionWrongKeyOnce(row.id)
			report.Unreadable++
		case resealRotated:
			swapped, err := s.swapActionCiphertext(ctx, row.id, row.enc, next)
			if err != nil {
				return report, err
			}
			if swapped {
				report.Rotated++
			}
		}
	}
	return report, nil
}

func (s *DatabaseStore) actionPasswordRows(ctx context.Context) ([]storedPassword, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT id, action_password_enc
		 FROM sage.databases WHERE action_password_enc IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing stored action passwords: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedPassword, error) {
		var p storedPassword
		err := r.Scan(&p.id, &p.enc)
		return p, err
	})
}

// resealActionPassword returns the value to write for resealRotated.
func (s *DatabaseStore) resealActionPassword(id int, enc []byte) ([]byte, string) {
	if !crypto.IsSealed(string(enc)) {
		return nil, resealUnreadable
	}
	aad := actionPasswordAAD(id)
	plain, kid, err := s.keyring.Open(string(enc), aad)
	if err != nil {
		return nil, resealUnreadable
	}
	if kid == s.keyring.ActiveKeyID() {
		return nil, resealCurrent
	}
	next, err := s.keyring.Seal(plain, aad)
	if err != nil {
		return nil, resealUnreadable
	}
	return []byte(next), resealRotated
}

func (s *DatabaseStore) swapActionCiphertext(
	ctx context.Context, id int, old, next []byte,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.databases
		    SET action_password_enc = $1, updated_at = now()
		  WHERE id = $2 AND action_password_enc = $3`, next, id, old)
	if err != nil {
		return false, fmt.Errorf("re-sealing the action password of database %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}
