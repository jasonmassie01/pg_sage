package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// leaseKey is one advisory key of a lease and the object identity it
// stands for. Name-only keys (legacy and unresolved targets) carry no kind
// or OID.
type leaseKey struct {
	key  string
	kind string
	oid  uint32
	name string
}

// typedKeys are the distinct keys of targets in a stable order, each with
// the identity of the target that contributed it first.
func typedKeys(targets []TypedTarget) []leaseKey {
	byKey := map[string]leaseKey{}
	for _, target := range targets {
		for _, key := range target.LeaseKeys() {
			if _, ok := byKey[key]; !ok {
				byKey[key] = leaseKey{key: key, kind: string(target.Kind),
					oid: target.OID, name: target.Canonical()}
			}
		}
	}
	keys := make([]leaseKey, 0, len(byKey))
	for _, key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].key < keys[j].key })
	return keys
}

// LeaseConflictError is a lease refused because another writer holds one
// of its objects. It names the holder when the lease row is readable.
type LeaseConflictError struct {
	ObjectKey  string
	Object     string
	Holder     string
	DecisionID int64
}

func (e *LeaseConflictError) Error() string {
	if e.Holder == "" {
		return fmt.Sprintf("%s: %s", ErrLeaseConflict, e.ObjectKey)
	}
	return fmt.Sprintf("%s: %s held by %s (decision %d)", ErrLeaseConflict, e.Object,
		e.Holder, e.DecisionID)
}

func (e *LeaseConflictError) Unwrap() error { return ErrLeaseConflict }

// conflict describes the active lease holding key. The description is
// best effort: an unreadable holder still reports the conflict.
func (m *PostgresLeaseManager) conflict(
	ctx context.Context, conn *pgxpool.Conn, key string,
) error {
	conflict := &LeaseConflictError{ObjectKey: key, Object: key}
	err := conn.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(actor, holder),
		decision_id, COALESCE(object_name, object_key)
		FROM sage.change_lease
		WHERE state='active' AND expires_at > now() AND object_key=$1
		  AND COALESCE(database_id, 0) = COALESCE($2::bigint, 0)
		ORDER BY acquired_at LIMIT 1`, key, m.databaseID).
		Scan(&conflict.Holder, &conflict.DecisionID, &conflict.Object)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w (holder unreadable: %v)", conflict, err)
	}
	return conflict
}
