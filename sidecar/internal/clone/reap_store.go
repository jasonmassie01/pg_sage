package clone

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The reaper's receipt queries (lab-05a). One predicate says which
// receipts a reaper may act on: past their TTL for its purpose, or, for
// any purpose, held by another process that stopped heartbeating. Every
// claim re-checks it in the statement that moves the receipt, so a
// holder heartbeating at the same moment always wins.

// ReapQuery is whose receipts a reaper pass acts on.
type ReapQuery struct {
	Scope   ReceiptScope
	Purpose string // the purpose whose expired receipts are reaped
	// Holder is the reaping process; "" turns liveness reaping off.
	Holder string
	// StaleAfter is how old a heartbeat is before its holder is dead.
	StaleAfter time.Duration
	// AsOf is the time heartbeats are judged at; zero is the database's.
	AsOf time.Time
}

const reapablePredicate = `deployment_id = $1::uuid AND adapter = $2 AND scope = $3
  AND status IN ('creating', 'create_uncertain', 'ready', 'destroying')
  AND ((purpose = $4 AND expires_at <= now())
       OR ($5::text <> '' AND holder_id IS NOT NULL AND heartbeat_at IS NOT NULL
           AND heartbeat_at < COALESCE($7::timestamptz, now()) - $6::interval
           AND (holder_id <> $5::text OR status = 'create_uncertain')))`

func (q ReapQuery) args(extra ...any) []any {
	var asOf *time.Time
	if !q.AsOf.IsZero() {
		asOf = &q.AsOf
	}
	return append([]any{q.Scope.DeploymentID, q.Scope.Adapter, q.Scope.Scope, q.Purpose,
		q.Holder, q.StaleAfter, asOf}, extra...)
}

func (q ReapQuery) validate() error {
	if err := q.Scope.validate(); err != nil {
		return err
	}
	if !purposes[q.Purpose] {
		return fmt.Errorf("%w: unknown purpose %q", ErrInvalidReceipt, q.Purpose)
	}
	if q.StaleAfter <= 0 {
		return fmt.Errorf("%w: a heartbeat staleness bound is required", ErrInvalidReceipt)
	}
	return nil
}

const reapableSQL = `/* pg_sage clone_receipt v2 */
SELECT ` + receiptColumns + ` FROM sage.clone_instances
WHERE ` + reapablePredicate + `
ORDER BY expires_at, id
LIMIT $8`

// Reapable returns up to MaxExpiredPerPass receipts q may act on, the
// soonest to expire first.
func (s *ReceiptStore) Reapable(ctx context.Context, q ReapQuery) ([]Receipt, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	if s == nil || s.pool == nil {
		return nil, ErrNoReceiptStore
	}
	rows, err := s.pool.Query(ctx, reapableSQL, q.args(MaxExpiredPerPass)...)
	if err != nil {
		return nil, fmt.Errorf("read reapable %s clone receipts: %w", q.Scope.Adapter, err)
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reapable clone receipt: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reapable %s clone receipts: %w", q.Scope.Adapter, err)
	}
	return out, nil
}

const claimForReapSQL = `/* pg_sage clone_receipt v2 */
UPDATE sage.clone_instances SET status = 'destroying', updated_at = now()
WHERE id = $8 AND status IN ('ready', 'destroying') AND ` + reapablePredicate + `
RETURNING ` + receiptColumns

const resolveFoundSQL = `/* pg_sage clone_receipt v2 */
UPDATE sage.clone_instances SET status = 'destroying', ref = $9, updated_at = now()
WHERE id = $8 AND status IN ('creating', 'create_uncertain') AND ` + reapablePredicate + `
RETURNING ` + receiptColumns

const closeUnfoundSQL = `/* pg_sage clone_receipt v2 */
UPDATE sage.clone_instances SET status = 'failed', updated_at = now()
WHERE id = $8 AND status IN ('creating', 'create_uncertain') AND expires_at <= now()
  AND ` + reapablePredicate + `
RETURNING ` + receiptColumns

// ClaimForReap moves a ready or destroying receipt q may still act on to
// destroying; false when it no longer may (its holder heartbeated, or
// another pass took it).
func (s *ReceiptStore) ClaimForReap(ctx context.Context, q ReapQuery, id int64) (Receipt,
	bool, error) {
	return s.claim(ctx, "claim", claimForReapSQL, q.args(id))
}

// ResolveFound records the resource a creating or uncertain receipt's
// name was found as, and claims it for teardown.
func (s *ReceiptStore) ResolveFound(ctx context.Context, q ReapQuery, id int64,
	ref string) (Receipt, bool, error) {
	if ref == "" || len(ref) > maxReceiptText {
		return Receipt{}, false, fmt.Errorf("%w: ref must be 1-%d characters",
			ErrInvalidReceipt, maxReceiptText)
	}
	return s.claim(ctx, "resolve", resolveFoundSQL, q.args(id, ref))
}

// CloseUnfound fails an expired creating or uncertain receipt whose
// resource does not exist; before its TTL the create may still land, so
// it is left as it is (false).
func (s *ReceiptStore) CloseUnfound(ctx context.Context, q ReapQuery, id int64) (bool,
	error) {
	_, ok, err := s.claim(ctx, "close", closeUnfoundSQL, q.args(id))
	return ok, err
}

func (s *ReceiptStore) claim(ctx context.Context, op, query string, args []any) (Receipt,
	bool, error) {
	if s == nil || s.pool == nil {
		return Receipt{}, false, ErrNoReceiptStore
	}
	got, err := scanReceipt(s.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("%s clone receipt %v: %w", op, args[7], err)
	}
	return got, true, nil
}

const heartbeatSQL = `/* pg_sage clone_receipt v2 */
UPDATE sage.clone_instances SET heartbeat_at = now()
WHERE id = ANY($1) AND holder_id = $2 AND status IN ('creating', 'ready')`

// Heartbeat says holder still holds the creating or ready receipts ids;
// it returns how many it refreshed.
func (s *ReceiptStore) Heartbeat(ctx context.Context, holder string, ids []int64) (int64,
	error) {
	if holder == "" || len(ids) == 0 {
		return 0, nil
	}
	if s == nil || s.pool == nil {
		return 0, ErrNoReceiptStore
	}
	tag, err := s.pool.Exec(ctx, heartbeatSQL, ids, holder)
	if err != nil {
		return 0, fmt.Errorf("heartbeat %d clone receipts: %w", len(ids), err)
	}
	return tag.RowsAffected(), nil
}
