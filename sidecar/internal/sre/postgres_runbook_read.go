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

// settle derives the status: retired, draft (latest unsigned), invalid
// (signature or content broken, or no longer valid) or signed (runnable).
func (rb *Runbook) settle() {
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
	rb.Runnable = rb.Status == RunbookSigned
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
// newest first.
func (s *PostgresStore) GetRunbook(ctx context.Context, scope Scope, id UUID) (Runbook,
	error) {
	if err := validateIDs(scope, id); err != nil {
		return Runbook{}, err
	}
	rb, err := scanRunbook(s.pool.QueryRow(ctx, `SELECT `+runbookColumns+`, `+
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
	rows, err := s.pool.Query(ctx, `SELECT `+versionColumns+`
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
	rb.settle()
	return rb, nil
}

// ListRunbooks lists this database's runbooks with their latest version,
// oldest first.
func (s *PostgresStore) ListRunbooks(ctx context.Context, scope Scope) ([]Runbook, error) {
	return s.listRunbooks(ctx, scope, "")
}

// RunnableRunbooks lists the runbooks that may run now: not retired, the
// latest version signed over its own, still matching content, and still
// valid against today's catalogs.
func (s *PostgresStore) RunnableRunbooks(ctx context.Context, scope Scope) ([]Runbook,
	error) {
	all, err := s.listRunbooks(ctx, scope,
		`AND r.retired_at IS NULL AND v.signed_at IS NOT NULL`)
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

// listRunbooks reads runbooks with their latest version; extra is a
// constant SQL predicate.
func (s *PostgresStore) listRunbooks(ctx context.Context, scope Scope,
	extra string) ([]Runbook, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runbookColumns+`, `+
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
		rb, err := scanRunbook(r, scope)
		rb.settle()
		return rb, err
	})
	if err != nil {
		return nil, storeErr(ctx, "list runbooks", err)
	}
	if out == nil {
		out = []Runbook{}
	}
	return out, nil
}
