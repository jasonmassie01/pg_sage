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

// MaxExpiredPerPass bounds one Reapable read.
const MaxExpiredPerPass = 100
