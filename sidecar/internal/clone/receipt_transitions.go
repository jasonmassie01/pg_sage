package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Receipt transitions after Record (spec §6.12): a create that failed or
// may have happened, the teardown, and the expired receipts a reaper acts
// on. Each transition is one guarded UPDATE, so it happens at most once
// and only from the states that allow it.

// ReceiptScope names whose receipts these are: one deployment, one
// adapter and one provider scope (for DLE, its endpoint). A reaper only
// ever acts on the receipts of its own scope.
type ReceiptScope struct {
	DeploymentID string
	Adapter      string
	Scope        string
}

func (s ReceiptScope) validate() error {
	if !uuidPattern.MatchString(s.DeploymentID) {
		return fmt.Errorf("%w: deployment_id must be a UUID", ErrInvalidReceipt)
	}
	if strings.TrimSpace(s.Adapter) == "" || strings.TrimSpace(s.Scope) == "" {
		return fmt.Errorf("%w: adapter and scope are required", ErrInvalidReceipt)
	}
	return nil
}

const markCreateFailedSQL = `/* pg_sage clone_receipt v1 */
UPDATE sage.clone_instances SET status = $2, updated_at = now()
WHERE id = $1 AND status = 'creating'
RETURNING ` + receiptColumns

// MarkCreateFailed records a provider create that did not return a clone:
// failed when the provider certainly made nothing, create_uncertain when
// it may have (a timeout or a cancelled request). Only a creating receipt
// moves.
func (s *ReceiptStore) MarkCreateFailed(ctx context.Context, id int64,
	uncertain bool) (Receipt, error) {
	status := StatusFailed
	if uncertain {
		status = StatusCreateUncertain
	}
	return s.transition(ctx, id, status, markCreateFailedSQL, status)
}

const markDestroyingSQL = `/* pg_sage clone_receipt v1 */
UPDATE sage.clone_instances SET status = 'destroying', updated_at = now()
WHERE id = $1 AND status IN ('ready', 'destroying')
RETURNING ` + receiptColumns

// MarkDestroying records that the clone's teardown starts. A destroying
// receipt may be marked again: a teardown that failed is retried.
func (s *ReceiptStore) MarkDestroying(ctx context.Context, id int64) (Receipt, error) {
	return s.transition(ctx, id, StatusDestroying, markDestroyingSQL)
}

const markDestroyedSQL = `/* pg_sage clone_receipt v1 */
UPDATE sage.clone_instances SET status = 'destroyed', updated_at = now()
WHERE id = $1 AND status = 'destroying'
RETURNING ` + receiptColumns

// MarkDestroyed closes the receipt once the provider destroyed the clone.
func (s *ReceiptStore) MarkDestroyed(ctx context.Context, id int64) (Receipt, error) {
	return s.transition(ctx, id, StatusDestroyed, markDestroyedSQL)
}

func (s *ReceiptStore) transition(ctx context.Context, id int64, to, query string,
	args ...any) (Receipt, error) {
	if s == nil || s.pool == nil {
		return Receipt{}, ErrNoReceiptStore
	}
	got, err := scanReceipt(s.pool.QueryRow(ctx, query, append([]any{id}, args...)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, s.stateError(ctx, id)
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("mark clone receipt %d %s: %w", id, to, err)
	}
	return got, nil
}

// MaxExpiredPerPass bounds one Expired read.
const MaxExpiredPerPass = 100

const expiredReceiptsSQL = `/* pg_sage clone_receipt v1 */
SELECT ` + receiptColumns + ` FROM sage.clone_instances
WHERE deployment_id = $1::uuid AND adapter = $2 AND scope = $3 AND purpose = $4
  AND status IN ('creating', 'create_uncertain', 'ready', 'destroying')
  AND expires_at <= now()
ORDER BY expires_at, id
LIMIT $5`

// Expired returns up to MaxExpiredPerPass receipts of scope and purpose
// that are past their expiry and not terminal (destroyed, failed), the
// oldest first.
func (s *ReceiptStore) Expired(ctx context.Context, scope ReceiptScope,
	purpose string) ([]Receipt, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	if !purposes[purpose] {
		return nil, fmt.Errorf("%w: unknown purpose %q", ErrInvalidReceipt, purpose)
	}
	if s == nil || s.pool == nil {
		return nil, ErrNoReceiptStore
	}
	rows, err := s.pool.Query(ctx, expiredReceiptsSQL, scope.DeploymentID, scope.Adapter,
		scope.Scope, purpose, MaxExpiredPerPass)
	if err != nil {
		return nil, fmt.Errorf("read expired %s clone receipts: %w", scope.Adapter, err)
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired clone receipt: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expired %s clone receipts: %w", scope.Adapter, err)
	}
	return out, nil
}
