package grants

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// fenceSQL holds the leader lease FOR SHARE while a revoke commits: a new
// leader's acquire waits for it, and a moved lease returns no row.
const fenceSQL = `/* pg_sage guard_grant_reconcile v1 */ SELECT 1 FROM sage.fleet_leader_lease
	WHERE scope = $1 AND holder = $2 AND epoch = $3 AND expires_at > now()
	FOR SHARE`

// holdFence checks the lease and holds it until release; an empty fence
// holds nothing.
func holdFence(ctx context.Context, f Fence) (func(), error) {
	if f.Holder == "" {
		return func() {}, nil
	}
	tx, err := f.Control.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("grants: checking the leader lease: %w", err)
	}
	release := func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }
	var one int
	err = tx.QueryRow(ctx, fenceSQL, f.Scope, f.Holder, f.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		release()
		return nil, ErrFenced
	}
	if err != nil {
		release()
		return nil, fmt.Errorf("grants: checking the leader lease: %w", err)
	}
	return release, nil
}

// ExpiryReport is one reconcile pass over a database.
type ExpiryReport struct {
	// Revoked are grants revoked (or revoke_incomplete) because they expired.
	Revoked []int64 `json:"revoked"`
	// Incomplete are the revoked grants another grantor's residue kept.
	Incomplete []int64 `json:"incomplete"`
	// Completed are revoke_incomplete grants whose residue is gone.
	Completed []int64 `json:"completed"`
	// Failed are grants whose revoke failed this pass; the next retries.
	Failed map[int64]error `json:"-"`
}

// MaxPass bounds the grants one pass revokes per database.
const MaxPass = 1000

// dueSQL uses guard_grants_open_idx (expires_at WHERE revoked_at IS NULL);
// relation rows go before schema rows so a schema's USAGE goes with its
// last relation.
const dueSQL = `/* pg_sage guard_grant_reconcile v1 */
SELECT id, object_kind FROM sage.guard_grants
WHERE revoked_at IS NULL AND expires_at <= now()
ORDER BY object_kind = 'schema', expires_at, id LIMIT $1`

// ExpireDue revokes every grant of the target whose expiry passed (at most
// limit per pass), as guard_revoke with cause expired, each under the
// leader fence, and re-checks revoke_incomplete grants whose residue may
// be gone. It stops with ErrFenced as soon as the lease moved.
func (m *Manager) ExpireDue(ctx context.Context, t Target, f Fence, limit int) (
	ExpiryReport, error) {
	rep := ExpiryReport{Failed: map[int64]error{}}
	if limit < 1 || limit > MaxPass {
		return rep, invalidf("limit must be 1-%d", MaxPass)
	}
	if f.Holder != "" && f.Control == nil {
		return rep, invalidf("a fenced pass needs the control database")
	}
	release, err := holdFence(ctx, f)
	if err != nil {
		return rep, err
	}
	release()
	due, err := dueGrants(ctx, t.Pool, limit)
	if err != nil {
		return rep, err
	}
	for _, d := range due {
		if err := m.expireOne(ctx, t, f, d, &rep); err != nil {
			return rep, err
		}
	}
	return rep, m.recheckIncomplete(ctx, t, f, limit, &rep)
}

type dueGrant struct {
	id   int64
	kind string
}

func dueGrants(ctx context.Context, q Querier, limit int) ([]dueGrant, error) {
	if q == nil {
		return nil, invalidf("the database has no pg_sage connection")
	}
	rows, err := q.Query(ctx, dueSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("grants: reading expired grants: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (dueGrant, error) {
		var d dueGrant
		return d, r.Scan(&d.id, &d.kind)
	})
}

// expireOne revokes one due grant. A grant a concurrent pass revoked, or a
// schema row whose schema is still in use, is skipped; only a moved lease
// stops the pass.
func (m *Manager) expireOne(ctx context.Context, t Target, f Fence, d dueGrant,
	rep *ExpiryReport) error {
	res, err := m.Revoke(ctx, RevokeRequest{Target: t, GrantID: d.id, Cause: CauseExpired,
		Fence: f})
	switch {
	case errors.Is(err, ErrFenced):
		return err
	case errors.Is(err, ErrNotActive):
		return nil
	case errors.Is(err, errSchemaInUse):
		return extendSchemaRow(ctx, t.Pool, d.id)
	case err != nil:
		rep.Failed[d.id] = err
		return nil
	}
	if d.kind == KindRelation {
		rep.Revoked = append(rep.Revoked, d.id)
	}
	if res.Grant.State == StateRevokeIncomplete {
		rep.Incomplete = append(rep.Incomplete, d.id)
	}
	return nil
}

// extendSchemaRow keeps a due schema row until the latest relation grant
// of its schema.
func extendSchemaRow(ctx context.Context, q Querier, id int64) error {
	_, err := q.Exec(ctx, `/* pg_sage guard_grant_reconcile v1 */
		UPDATE sage.guard_grants s SET expires_at = r.latest
		FROM (SELECT max(o.expires_at) AS latest FROM sage.guard_grants o, sage.guard_grants x
		      WHERE x.id = $1 AND o.principal_id = x.principal_id
		        AND o.schema_oid = x.schema_oid AND o.object_kind = 'relation'
		        AND o.state = 'active' AND o.revoked_at IS NULL) r
		WHERE s.id = $1 AND r.latest IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("grants: extending schema grant %d: %w", id, err)
	}
	return nil
}
