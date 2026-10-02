package sre

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Runbook writes. Every write locks the runbook row, so edits, signatures
// and retirement of one runbook are serialized: a signature can only bind
// the latest version, and an edit always leaves an unsigned latest version.

// runbookErr keeps the runbook sentinels and classifies everything else
// like every store error.
func runbookErr(ctx context.Context, op string, err error) error {
	for _, sentinel := range []error{ErrHashMismatch, ErrAlreadySigned, ErrRetired} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return storeErr(ctx, op, err)
}

// CreateRunbook stores a new runbook whose version 1 is an unsigned draft.
func (s *PostgresStore) CreateRunbook(ctx context.Context, scope Scope,
	d RunbookInput) (Runbook, error) {
	if err := scope.Validate(); err != nil {
		return Runbook{}, err
	}
	if err := d.validate(); err != nil {
		return Runbook{}, err
	}
	id := NewUUID()
	err := s.inTx(ctx, "create runbook", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_runbooks
			(deployment_id, database_id, id, created_by) VALUES ($1, $2, $3, $4)`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id),
			d.Actor); err != nil {
			return err
		}
		return insertRunbookVersion(ctx, tx, scope, id, 1, d)
	})
	if err != nil {
		return Runbook{}, runbookErr(ctx, "create runbook", err)
	}
	return s.GetRunbook(ctx, scope, id)
}

func insertRunbookVersion(ctx context.Context, tx pgx.Tx, scope Scope, id UUID,
	version int, d RunbookInput) error {
	canonical, err := d.Definition.Canonical()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	hash, err := d.Definition.Hash()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	sum, _ := hex.DecodeString(hash)
	_, err = tx.Exec(ctx, `INSERT INTO sage.sre_runbook_versions
		(deployment_id, database_id, runbook_id, version, name, definition, content_hash,
		 source, source_text, compiled_by, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11)`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), version,
		d.Definition.Name, string(canonical), sum, d.source(), d.SourceText, d.CompiledBy,
		d.Actor)
	return err
}

// lockedRunbook is the locked runbook row a write decides on.
type lockedRunbook struct {
	latest  int
	retired bool
}

func lockRunbook(ctx context.Context, tx pgx.Tx, scope Scope, id UUID) (lockedRunbook,
	error) {
	var r lockedRunbook
	err := tx.QueryRow(ctx, `SELECT latest_version, retired_at IS NOT NULL
		FROM sage.sre_runbooks
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3 FOR UPDATE`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)).
		Scan(&r.latest, &r.retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrRunbookNotFound
	}
	return r, err
}

// ReviseRunbook appends an unsigned draft version on top of baseVersion,
// which must be the latest (optimistic concurrency). Editing invalidates
// the signature: the runbook does not run until the new version is signed.
func (s *PostgresStore) ReviseRunbook(ctx context.Context, scope Scope, id UUID,
	baseVersion int, d RunbookInput) (Runbook, error) {
	if err := validateIDs(scope, id); err != nil {
		return Runbook{}, err
	}
	if err := d.validate(); err != nil {
		return Runbook{}, err
	}
	err := s.inTx(ctx, "revise runbook", func(tx pgx.Tx) error {
		r, err := lockRunbook(ctx, tx, scope, id)
		switch {
		case err != nil:
			return err
		case r.retired:
			return ErrRetired
		case r.latest != baseVersion:
			return fmt.Errorf("%w: the latest version is %d, not %d", ErrVersionConflict,
				r.latest, baseVersion)
		}
		if err := insertRunbookVersion(ctx, tx, scope, id, r.latest+1, d); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE sage.sre_runbooks
			SET latest_version = $4, updated_at = clock_timestamp()
			WHERE deployment_id = $1 AND database_id = $2 AND id = $3`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id), r.latest+1)
		return err
	})
	if err != nil {
		return Runbook{}, runbookErr(ctx, "revise runbook", err)
	}
	return s.GetRunbook(ctx, scope, id)
}

func (sig RunbookSignature) validate() ([]byte, error) {
	if sig.Role != "admin" {
		return nil, fmt.Errorf("%w: only an admin may sign a runbook", ErrInvalidRequest)
	}
	if sig.Version <= 0 {
		return nil, fmt.Errorf("%w: a signature names a version", ErrInvalidRequest)
	}
	if err := checkText("signer", sig.Signer, true, 128); err != nil {
		return nil, err
	}
	return hashBytes(sig.ContentHash)
}

func hashBytes(h string) ([]byte, error) {
	sum, err := hex.DecodeString(strings.ToLower(h))
	if err != nil || len(sum) != 32 {
		return nil, fmt.Errorf("%w: a content hash is 64 hex characters", ErrInvalidRequest)
	}
	return sum, nil
}

// SignRunbook has an admin sign the latest version. The signature binds
// the content hash the signer reviewed, which must equal both the stored
// hash and the hash of the stored definition, and the definition must
// still validate.
func (s *PostgresStore) SignRunbook(ctx context.Context, scope Scope, id UUID,
	sig RunbookSignature) (Runbook, error) {
	if err := validateIDs(scope, id); err != nil {
		return Runbook{}, err
	}
	reviewed, err := sig.validate()
	if err != nil {
		return Runbook{}, err
	}
	err = s.inTx(ctx, "sign runbook", func(tx pgx.Tx) error {
		r, err := lockRunbook(ctx, tx, scope, id)
		switch {
		case err != nil:
			return err
		case r.retired:
			return ErrRetired
		case r.latest != sig.Version:
			return fmt.Errorf("%w: only the latest version (%d) can be signed",
				ErrVersionConflict, r.latest)
		}
		v, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionColumns+`
			FROM sage.sre_runbook_versions
			WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3
			  AND version = $4`, string(scope.DeploymentID), string(scope.DatabaseID),
			string(id), sig.Version))
		if err != nil {
			return err
		}
		if err := signable(v, hex.EncodeToString(reviewed)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE sage.sre_runbook_versions
			SET signed_by = $5, signer_role = $6, signed_hash = content_hash,
			    signed_at = clock_timestamp()
			WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3
			  AND version = $4`, string(scope.DeploymentID), string(scope.DatabaseID),
			string(id), sig.Version, sig.Signer, sig.Role)
		return err
	})
	if err != nil {
		return Runbook{}, runbookErr(ctx, "sign runbook", err)
	}
	return s.GetRunbook(ctx, scope, id)
}

func signable(v RunbookVersion, reviewed string) error {
	switch {
	case v.SignedAt != nil:
		return ErrAlreadySigned
	case !v.contentValid || v.ContentHash != reviewed:
		return fmt.Errorf("%w: version %d hashes to %s", ErrHashMismatch, v.Version,
			v.ContentHash)
	}
	if problems := problemsOf(v); problems != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, problems)
	}
	return nil
}

// RetireRunbook stops a runbook for good: it never runs again and cannot
// be edited or signed.
func (s *PostgresStore) RetireRunbook(ctx context.Context, scope Scope, id UUID,
	actor string) (Runbook, error) {
	if err := validateIDs(scope, id); err != nil {
		return Runbook{}, err
	}
	if err := checkText("actor", actor, true, 128); err != nil {
		return Runbook{}, err
	}
	err := s.inTx(ctx, "retire runbook", func(tx pgx.Tx) error {
		r, err := lockRunbook(ctx, tx, scope, id)
		switch {
		case err != nil:
			return err
		case r.retired:
			return ErrRetired
		}
		_, err = tx.Exec(ctx, `UPDATE sage.sre_runbooks
			SET retired_by = $4, retired_at = clock_timestamp(),
			    updated_at = clock_timestamp()
			WHERE deployment_id = $1 AND database_id = $2 AND id = $3`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id), actor)
		return err
	})
	if err != nil {
		return Runbook{}, runbookErr(ctx, "retire runbook", err)
	}
	return s.GetRunbook(ctx, scope, id)
}
