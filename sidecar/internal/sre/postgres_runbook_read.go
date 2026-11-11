package sre

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Runbook reads. Every read re-derives the version's hash from its stored
// definition: a definition changed behind the store (the version guard
// bypassed) no longer matches its hash, so its signature is invalid and it
// never runs.

// versionCols lists a version's columns, each prefixed with alias.
func versionCols(alias string) string {
	return fmt.Sprintf(`%[1]sversion, %[1]sname, %[1]sdefinition::text, %[1]scontent_hash,
	%[1]ssource, COALESCE(%[1]ssource_text, ''), COALESCE(%[1]scompiled_by, ''),
	%[1]screated_by, %[1]screated_at, COALESCE(%[1]ssigned_by, ''),
	COALESCE(%[1]ssigner_role, ''), %[1]ssigned_hash, %[1]ssigned_at`, alias)
}

var versionColumns = versionCols("")

const runbookColumns = `r.id::text, r.latest_version, r.created_by, r.created_at,
	r.updated_at, COALESCE(r.retired_by, ''), r.retired_at`

// maxRunbooks bounds one database's runbook list.
const maxRunbooks = 500

// versionFields are the scan targets of versionCols.
type versionFields struct {
	v      RunbookVersion
	def    string
	hash   []byte
	signed []byte
}

func (f *versionFields) targets() []any {
	return []any{&f.v.Version, &f.v.Name, &f.def, &f.hash, &f.v.Source, &f.v.SourceText,
		&f.v.CompiledBy, &f.v.CreatedBy, &f.v.CreatedAt, &f.v.SignedBy, &f.v.SignerRole,
		&f.signed, &f.v.SignedAt}
}

// version decodes the stored definition and checks its hash and signature.
func (f *versionFields) version() RunbookVersion {
	v := f.v
	v.ContentHash = hex.EncodeToString(f.hash)
	if f.signed != nil {
		v.signedHash = hex.EncodeToString(f.signed)
	}
	if d, err := runbook.Decode([]byte(f.def)); err == nil {
		v.Definition = d
		h, herr := d.Hash()
		v.contentValid = herr == nil && h == v.ContentHash
	}
	v.SignatureValid = v.SignedAt != nil && v.contentValid && v.signedHash == v.ContentHash
	return v
}

func scanVersion(row pgx.Row) (RunbookVersion, error) {
	var f versionFields
	if err := row.Scan(f.targets()...); err != nil {
		return RunbookVersion{}, err
	}
	return f.version(), nil
}

func problemsOf(v RunbookVersion) runbook.Problems {
	return runbook.Validate(v.Definition, runbookVocab())
}

// settle derives the latest version's status (retired, draft when
// unsigned, invalid when its signature or content is broken or it no
// longer validates, else signed) and the running version from signed,
// the runbook's versions newest first.
func (rb *Runbook) settle(signed []RunbookVersion) {
	v := rb.Latest
	rb.Problems = problemsOf(v)
	switch {
	case rb.RetiredAt != nil:
		rb.Status = RunbookRetired
	case v.SignedAt == nil:
		rb.Status = RunbookDraft
	case !v.SignatureValid || rb.Problems != nil:
		rb.Status = RunbookInvalid
	default:
		rb.Status = RunbookSigned
	}
	rb.Running, rb.RunningVersion = nil, 0
	if rb.RetiredAt == nil {
		rb.Running = runningVersion(signed)
	}
	if rb.Running != nil {
		rb.RunningVersion = rb.Running.Version
	}
	rb.Runnable = rb.Running != nil
}

// runningVersion is the newest version whose signature is valid, when it
// still validates against today's catalogs. Unsigned drafts and versions
// whose content no longer matches the signed hash are skipped; an older
// version never stands in for a validly signed one that no longer
// validates.
func runningVersion(newestFirst []RunbookVersion) *RunbookVersion {
	for _, v := range newestFirst {
		if !v.SignatureValid {
			continue
		}
		if problemsOf(v) != nil {
			return nil
		}
		return &v
	}
	return nil
}

// scanRunbook reads runbookColumns followed by versionCols("v.").
func scanRunbook(row pgx.Row, scope Scope) (Runbook, error) {
	rb := Runbook{Scope: scope}
	var id string
	var f versionFields
	dest := append([]any{&id, &rb.LatestVersion, &rb.CreatedBy, &rb.CreatedAt,
		&rb.UpdatedAt, &rb.RetiredBy, &rb.RetiredAt}, f.targets()...)
	if err := row.Scan(dest...); err != nil {
		return rb, err
	}
	rb.ID, rb.Latest = UUID(id), f.version()
	return rb, nil
}

// GetRunbook returns one runbook of this database with every version,
// newest first, read from one snapshot.
func (s *PostgresStore) GetRunbook(ctx context.Context, scope Scope, id UUID) (Runbook,
	error) {
	if err := validateIDs(scope, id); err != nil {
		return Runbook{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return Runbook{}, storeErr(ctx, "get runbook", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rb, err := scanRunbook(tx.QueryRow(ctx, `SELECT `+runbookColumns+`, `+
		versionCols("v.")+`
		FROM sage.sre_runbooks r
		JOIN sage.sre_runbook_versions v ON v.deployment_id = r.deployment_id
		 AND v.database_id = r.database_id AND v.runbook_id = r.id
		 AND v.version = r.latest_version
		WHERE r.deployment_id = $1 AND r.database_id = $2 AND r.id = $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)), scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runbook{}, ErrRunbookNotFound
	}
	if err != nil {
		return Runbook{}, storeErr(ctx, "get runbook", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+versionColumns+`
		FROM sage.sre_runbook_versions
		WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3
		ORDER BY version DESC`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id))
	if err != nil {
		return Runbook{}, storeErr(ctx, "runbook versions", err)
	}
	rb.Versions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (RunbookVersion,
		error) {
		return scanVersion(r)
	})
	if err != nil {
		return Runbook{}, storeErr(ctx, "runbook versions", err)
	}
	rb.settle(rb.Versions)
	return rb, nil
}

// ListRunbooks lists this database's runbooks with their latest version,
// oldest first.
func (s *PostgresStore) ListRunbooks(ctx context.Context, scope Scope) ([]Runbook, error) {
	return s.listRunbooks(ctx, scope, "")
}

// RunnableRunbooks lists the runbooks that may run now: not retired, with
// a running version (Running): the newest version signed over its own,
// still matching content, and still valid against today's catalogs. A
// newer unsigned draft does not stop it.
func (s *PostgresStore) RunnableRunbooks(ctx context.Context, scope Scope) ([]Runbook,
	error) {
	all, err := s.listRunbooks(ctx, scope, `AND r.retired_at IS NULL`)
	if err != nil {
		return nil, err
	}
	out := []Runbook{}
	for _, rb := range all {
		if rb.Runnable {
			out = append(out, rb)
		}
	}
	return out, nil
}

// listRunbooks reads runbooks with their latest and running versions;
// extra is a constant SQL predicate. Both reads share one snapshot, so a
// sign or retire committing between them never leaves a retired runbook
// running or a running version newer than the latest.
func (s *PostgresStore) listRunbooks(ctx context.Context, scope Scope,
	extra string) ([]Runbook, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, storeErr(ctx, "list runbooks", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	out, err := latestRunbooks(ctx, tx, scope, extra)
	if err != nil {
		return nil, err
	}
	signed, err := signedVersions(ctx, tx, scope, out)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].settle(signed[out[i].ID])
	}
	if out == nil {
		out = []Runbook{}
	}
	return out, nil
}

// latestRunbooks reads runbooks with their latest version.
func latestRunbooks(ctx context.Context, tx pgx.Tx, scope Scope,
	extra string) ([]Runbook, error) {
	rows, err := tx.Query(ctx, `SELECT `+runbookColumns+`, `+
		versionCols("v.")+`
		FROM sage.sre_runbooks r
		JOIN sage.sre_runbook_versions v ON v.deployment_id = r.deployment_id
		 AND v.database_id = r.database_id AND v.runbook_id = r.id
		 AND v.version = r.latest_version
		WHERE r.deployment_id = $1 AND r.database_id = $2 `+extra+`
		ORDER BY r.created_at, r.id LIMIT $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), maxRunbooks)
	if err != nil {
		return nil, storeErr(ctx, "list runbooks", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Runbook, error) {
		return scanRunbook(r, scope)
	})
	if err != nil {
		return nil, storeErr(ctx, "list runbooks", err)
	}
	return out, nil
}

// signedVersions reads the signed versions of the listed, unretired
// runbooks, newest first per runbook, to find each one's running version.
func signedVersions(ctx context.Context, tx pgx.Tx, scope Scope,
	rbs []Runbook) (map[UUID][]RunbookVersion, error) {
	ids := []string{}
	for _, rb := range rbs {
		if rb.RetiredAt == nil {
			ids = append(ids, string(rb.ID))
		}
	}
	out := map[UUID][]RunbookVersion{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT runbook_id::text, `+versionColumns+`
		FROM sage.sre_runbook_versions
		WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = ANY($3::uuid[])
		  AND signed_at IS NOT NULL
		ORDER BY runbook_id, version DESC`,
		string(scope.DeploymentID), string(scope.DatabaseID), ids)
	if err != nil {
		return nil, storeErr(ctx, "signed runbook versions", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var f versionFields
		if err := rows.Scan(append([]any{&id}, f.targets()...)...); err != nil {
			return nil, storeErr(ctx, "signed runbook versions", err)
		}
		out[UUID(id)] = append(out[UUID(id)], f.version())
	}
	if err := rows.Err(); err != nil {
		return nil, storeErr(ctx, "signed runbook versions", err)
	}
	return out, nil
}
