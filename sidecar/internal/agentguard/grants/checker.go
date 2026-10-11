package grants

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Checker is the grant side of the gate's D5 (decide.ObjectChecker) and
// D10 (decide.Leases), shared with the broker.
type Checker struct {
	// Resolve maps a fleet database name to its target (pool and id).
	Resolve func(ctx context.Context, database string) (Target, error)
	// Unmasked reports an agents.unmask entry; nil unmasks nothing.
	Unmasked func(principalID string, col classify.Column) bool
}

// LeaseActive reports whether grant grantID of principalID is active and
// unexpired in database's registry, and no revoke of the same object left
// another grantor's residue (D10: the lease decides, even while the
// database grant still exists).
func (c *Checker) LeaseActive(ctx context.Context, principalID, grantID,
	database string) (bool, error) {
	id, err := strconv.ParseInt(grantID, 10, 64)
	if err != nil || id <= 0 {
		return false, invalidf("grant id %q", grantID)
	}
	t, err := c.Resolve(ctx, database)
	if err != nil {
		return false, err
	}
	var ok bool
	err = t.Pool.QueryRow(ctx, `/* pg_sage guard_lease v1 */
		SELECT g.state = 'active' AND g.revoked_at IS NULL AND g.expires_at > now()
		  AND NOT EXISTS (SELECT 1 FROM sage.guard_grants o
		    WHERE o.principal_id = g.principal_id AND o.object_oid = g.object_oid
		      AND o.state = 'revoke_incomplete')
		FROM sage.guard_grants g WHERE g.id = $1 AND g.principal_id = $2`,
		id, principalID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("grants: reading lease %d: %w", id, err)
	}
	return ok, nil
}

// objectLease is what the registry holds for one principal and relation.
type objectLease struct {
	covered    map[string]bool // columns of active, unexpired grants
	expired    map[string]bool // columns of active grants past their expiry
	whole      bool            // some active, unexpired grant on the relation
	incomplete bool            // a revoke left another grantor's residue
}

func readLease(ctx context.Context, q Querier, principalID string, oid uint32) (
	objectLease, error) {
	l := objectLease{covered: map[string]bool{}, expired: map[string]bool{}}
	rows, err := q.Query(ctx, `/* pg_sage guard_lease v1 */
		SELECT state, expires_at > now(), columns FROM sage.guard_grants
		WHERE principal_id = $1 AND object_oid = $2::int8::oid AND state <> 'revoked'`,
		principalID, int64(oid))
	if err != nil {
		return l, fmt.Errorf("grants: reading grants of %d: %w", oid, err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var live bool
		var cols []string
		if err := rows.Scan(&state, &live, &cols); err != nil {
			return l, fmt.Errorf("grants: reading grants of %d: %w", oid, err)
		}
		into := l.expired
		switch {
		case state == StateRevokeIncomplete:
			l.incomplete = true
			continue
		case live:
			into, l.whole = l.covered, true
		}
		for _, c := range cols {
			into[c] = true
		}
	}
	return l, rows.Err()
}

// CheckObjects is D5 and D10 for a brokered request's objects: each
// relation resolves to an OID; each column is grantable in env
// (classify.Grantable: never secret, only confirmed clean in stage/prod)
// and covered by an active, unexpired grant; an object with
// revoke_incomplete residue is denied. Denials are *agentguard.DeniedError.
func (c *Checker) CheckObjects(ctx context.Context, p agentguard.Principal,
	database string, env envbind.Env, objects []decide.Object) error {
	if len(objects) == 0 {
		return nil
	}
	t, err := c.Resolve(ctx, database)
	if err != nil {
		return err
	}
	byRel := map[[2]string][]string{}
	var order [][2]string
	for _, o := range objects {
		k := [2]string{o.Schema, o.Relation}
		if _, seen := byRel[k]; !seen {
			order = append(order, k)
		}
		byRel[k] = append(byRel[k], o.Column)
	}
	for _, k := range order {
		if err := c.checkRelation(ctx, t, p.ID, effectiveEnv(env), k, byRel[k]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Checker) checkRelation(ctx context.Context, t Target, principalID string,
	env envbind.Env, k [2]string, cols []string) error {
	rel, err := resolveRelation(ctx, t.Pool, k[0], k[1])
	if errors.Is(err, agentguard.ErrNotFound) {
		return deny(decide.ReasonClassification, "", "relation %s does not exist",
			ident(k[0], k[1]))
	}
	if err != nil {
		return err
	}
	lease, err := readLease(ctx, t.Pool, principalID, rel.oid)
	if err != nil {
		return err
	}
	if lease.incomplete {
		return deny(decide.ReasonLeaseExpired, "", "a revoke on %s left another grantor's "+
			"privilege (revoke_incomplete); that grantor must revoke it", rel.qualified())
	}
	rc, err := classify.NewStore(t.Pool).Lookup(ctx, rel.oid)
	if err != nil {
		return fmt.Errorf("grants: classification of %s: %w", rel.qualified(), err)
	}
	for _, col := range cols {
		if err := c.checkColumn(rel, rc, lease, env, principalID, col); err != nil {
			return err
		}
	}
	return nil
}

func (c *Checker) checkColumn(rel *relation, rc classify.RelationClasses, lease objectLease,
	env envbind.Env, principalID, col string) error {
	if col == "" {
		if lease.whole {
			return nil
		}
		return leaseDenial(rel, lease.expired, "")
	}
	var live *classify.Column
	for i := range rel.columns {
		if rel.columns[i].col.Name == col {
			live = &rel.columns[i].col
		}
	}
	if live == nil {
		return deny(decide.ReasonClassification, "", "column %s of %s does not exist", col,
			rel.qualified())
	}
	unmasked := func(cl classify.Column) bool {
		return c.Unmasked != nil && c.Unmasked(principalID, cl)
	}
	if _, excl := classify.Grantable(env, []classify.Column{*live}, rc,
		unmasked); len(excl) > 0 {
		return deny(decide.ReasonClassification, "", "column %s of %s is %s in %s", col,
			rel.qualified(), excl[0].Reason, env)
	}
	if lease.covered[col] {
		return nil
	}
	return leaseDenial(rel, lease.expired, col)
}

// leaseDenial is an uncovered column: agent_lease_expired when an expired
// grant covered it, else outside the principal's grants (D5).
func leaseDenial(rel *relation, expired map[string]bool, col string) error {
	if (col == "" && len(expired) > 0) || expired[col] {
		return deny(decide.ReasonLeaseExpired, "", "the grant on %s has expired; request "+
			"it again with agent_request_capability", rel.qualified())
	}
	what := rel.qualified()
	if col != "" {
		what = "column " + col + " of " + what
	}
	return deny(decide.ReasonClassification, "", "%s is not within the principal's grants; "+
		"request it with agent_request_capability", what)
}
