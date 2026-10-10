package grants

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// lockGrants serializes grant changes of one principal across sidecars
// (the contracts' lease target), then bounds catalog locks to 2 s.
func lockGrants(ctx context.Context, tx pgx.Tx, principalID string) error {
	steps := []struct {
		sql  string
		args []any
	}{
		{"SET LOCAL lock_timeout = '30s'", nil},
		{"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
			[]any{"pg_sage guard_grant:" + principalID}},
		{"SET LOCAL lock_timeout = '2s'", nil},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, "/* pg_sage guard_grant v1 */ "+st.sql,
			st.args...); err != nil {
			return fmt.Errorf("grants: locking grants of %s: %w", principalID, err)
		}
	}
	return nil
}

// activeInSchema counts the principal's active relation grants in a
// schema, other than except.
func activeInSchema(ctx context.Context, q Querier, principalID string, schemaOID uint32,
	except int64) (int, error) {
	var n int
	err := q.QueryRow(ctx, `/* pg_sage guard_revoke v1 */
		SELECT count(*) FROM sage.guard_grants
		WHERE principal_id = $1 AND schema_oid = $2::int8::oid AND object_kind = 'relation'
		  AND state = 'active' AND revoked_at IS NULL AND id <> $3`,
		principalID, int64(schemaOID), except).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("grants: counting grants in schema %d: %w", schemaOID, err)
	}
	return n, nil
}

// revokeSchemaWithLast revokes the schema USAGE row when g was the last
// active relation grant of the principal in that schema.
func (r *revokeRun) revokeSchemaWithLast(ctx context.Context, tx pgx.Tx, g Grant) error {
	n, err := activeInSchema(ctx, tx, g.PrincipalID, g.SchemaOID, g.ID)
	if err != nil || n > 0 {
		return err
	}
	s, err := scanGrant(tx.QueryRow(ctx, openSchemaRowSQL, g.PrincipalID,
		int64(g.SchemaOID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("grants: reading schema USAGE of %s: %w", g.ObjectName, err)
	}
	_, err = r.revokeSchema(ctx, tx, s)
	return err
}

// revokeSchema revokes one schema USAGE row GRANTED BY its grantor.
func (r *revokeRun) revokeSchema(ctx context.Context, tx pgx.Tx, s Grant) (string, error) {
	var name string
	err := tx.QueryRow(ctx, `/* pg_sage guard_revoke v1 */
		SELECT nspname::text FROM pg_catalog.pg_namespace WHERE oid = $1::int8::oid`,
		int64(s.ObjectOID)).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "the schema was dropped", r.schemaRevoked(ctx, tx, s)
	}
	if err != nil {
		return "", fmt.Errorf("grants: resolving schema %d: %w", s.ObjectOID, err)
	}
	stmt := fmt.Sprintf("REVOKE USAGE ON SCHEMA %s FROM %s GRANTED BY %s", ident(name),
		ident(r.broker), ident(s.Grantor))
	if err := r.exec(ctx, tx, stmt); err != nil {
		return "", err
	}
	return "", r.schemaRevoked(ctx, tx, s)
}

// schemaRevoked remembers a schema row revoked with its relation; its row
// is marked when the action id is known.
func (r *revokeRun) schemaRevoked(_ context.Context, _ pgx.Tx, s Grant) error {
	r.schemaRows = append(r.schemaRows, s.ID)
	return nil
}

// markRevoked closes the row (revoked, or revoke_incomplete with the
// residue named) and the schema rows revoked with it.
func (r *revokeRun) markRevoked(ctx context.Context, tx pgx.Tx, g Grant, detail string,
	actionID int64) error {
	state := StateRevoked
	if len(r.result.Residue) > 0 {
		state = StateRevokeIncomplete
	}
	const sql = `/* pg_sage guard_revoke v1 */
		UPDATE sage.guard_grants SET revoked_at = now(), state = $2,
		  revoke_action_id = $3, revoke_detail = NULLIF($4, '')
		WHERE id = $1 RETURNING ` + grantColumns
	updated, err := scanGrant(tx.QueryRow(ctx, sql, g.ID, state, actionID, detail))
	if err != nil {
		return fmt.Errorf("grants: closing grant %d: %w", g.ID, err)
	}
	r.result.Grant = updated
	for _, id := range r.schemaRows {
		if id == g.ID {
			continue
		}
		s, err := scanGrant(tx.QueryRow(ctx, sql, id, StateRevoked, actionID, ""))
		if err != nil {
			return fmt.Errorf("grants: closing schema grant %d: %w", id, err)
		}
		r.result.Schema = &s
	}
	return nil
}
