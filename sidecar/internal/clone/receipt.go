package clone

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Receipt statuses (sage.clone_instances.status).
const (
	StatusCreating        = "creating"
	StatusCreateUncertain = "create_uncertain"
	StatusReady           = "ready"
	StatusDestroying      = "destroying"
	StatusDestroyed       = "destroyed"
	StatusFailed          = "failed"
)

// Receipt purposes.
const (
	PurposeRehearsal    = "rehearsal"
	PurposeSandbox      = "sandbox"
	PurposeDrill        = "drill"
	PurposeGameday      = "gameday"
	PurposeBench        = "bench"
	PurposeMaskedParent = "masked_parent"
)

// Receipt errors. Each is distinguishable with errors.Is.
var (
	ErrInvalidReceipt  = errors.New("clone: invalid receipt")
	ErrReceiptExists   = errors.New("clone: a receipt with this adapter, scope and name exists")
	ErrReceiptNotFound = errors.New("clone: receipt not found")
	ErrReceiptState    = errors.New("clone: receipt is not in a state that allows this")
	ErrNoReceiptStore  = errors.New("clone: no control database for receipts")
)

// Receipt is pg_sage's durable record of one clone it creates (spec §6.12):
// written before the provider call, completed with the provider resource
// id once the clone is ready. A branch environment label is verified only
// by an active receipt (§6.5).
type Receipt struct {
	ID               int64     `json:"id"`
	DeploymentID     string    `json:"deployment_id"`
	Adapter          string    `json:"adapter"`
	Scope            string    `json:"scope"`
	Ref              string    `json:"ref,omitempty"`
	Name             string    `json:"name"`
	Purpose          string    `json:"purpose"`
	PrincipalID      string    `json:"principal_id,omitempty"`
	SourceDatabaseID string    `json:"source_database_id,omitempty"`
	Masked           bool      `json:"masked"`
	Status           string    `json:"status"`
	ExpiresAt        time.Time `json:"expires_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ProviderRef is the provider resource id the receipt names, as
// "<adapter>:<ref>"; empty until the provider returned one.
func (r Receipt) ProviderRef() string {
	if r.Ref == "" {
		return ""
	}
	return r.Adapter + ":" + r.Ref
}

// ReceiptStore keeps receipts in the control database.
type ReceiptStore struct{ pool *pgxpool.Pool }

// NewReceiptStore returns the receipt store on pool (the control database).
func NewReceiptStore(pool *pgxpool.Pool) *ReceiptStore { return &ReceiptStore{pool: pool} }

var (
	uuidPattern = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	purposes = map[string]bool{PurposeRehearsal: true, PurposeSandbox: true,
		PurposeDrill: true, PurposeGameday: true, PurposeBench: true,
		PurposeMaskedParent: true}
)

const maxReceiptText = 200

func validateReceipt(r Receipt, now time.Time) error {
	if !uuidPattern.MatchString(r.DeploymentID) {
		return fmt.Errorf("%w: deployment_id must be a UUID", ErrInvalidReceipt)
	}
	for field, v := range map[string]string{"adapter": r.Adapter, "scope": r.Scope,
		"name": r.Name} {
		if strings.TrimSpace(v) == "" || len(v) > maxReceiptText {
			return fmt.Errorf("%w: %s must be 1-%d characters", ErrInvalidReceipt, field,
				maxReceiptText)
		}
	}
	if !purposes[r.Purpose] {
		return fmt.Errorf("%w: unknown purpose %q", ErrInvalidReceipt, r.Purpose)
	}
	if !r.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expires_at must be in the future", ErrInvalidReceipt)
	}
	if r.SourceDatabaseID != "" && !uuidPattern.MatchString(r.SourceDatabaseID) {
		return fmt.Errorf("%w: source_database_id must be a UUID", ErrInvalidReceipt)
	}
	return nil
}

const receiptColumns = `id, deployment_id::text, adapter, scope, COALESCE(ref, ''), name,
	purpose, COALESCE(principal_id, ''), COALESCE(source_database_id::text, ''), masked,
	status, expires_at, created_at, updated_at`

func scanReceipt(row pgx.Row) (Receipt, error) {
	var r Receipt
	err := row.Scan(&r.ID, &r.DeploymentID, &r.Adapter, &r.Scope, &r.Ref, &r.Name,
		&r.Purpose, &r.PrincipalID, &r.SourceDatabaseID, &r.Masked, &r.Status,
		&r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

const recordReceiptSQL = `/* pg_sage clone_receipt v1 */
INSERT INTO sage.clone_instances (deployment_id, adapter, scope, name, purpose,
    principal_id, source_database_id, masked, status, expires_at)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::uuid, $8, 'creating', $9)
RETURNING ` + receiptColumns

// Record writes the receipt (status creating) in one INSERT. Call it
// before the provider call, so a crash after the provider acted still
// leaves a record of what to adopt or destroy.
func (s *ReceiptStore) Record(ctx context.Context, r Receipt) (Receipt, error) {
	if err := validateReceipt(r, time.Now()); err != nil {
		return Receipt{}, err
	}
	if s.pool == nil {
		return Receipt{}, ErrNoReceiptStore
	}
	got, err := scanReceipt(s.pool.QueryRow(ctx, recordReceiptSQL, r.DeploymentID,
		r.Adapter, r.Scope, r.Name, r.Purpose, r.PrincipalID, r.SourceDatabaseID, r.Masked,
		r.ExpiresAt))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Receipt{}, fmt.Errorf("%w: %s/%s/%s", ErrReceiptExists, r.Adapter, r.Scope,
			r.Name)
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("record clone receipt %s: %w", r.Name, err)
	}
	return got, nil
}

const markReadySQL = `/* pg_sage clone_receipt v1 */
UPDATE sage.clone_instances SET status = 'ready', ref = $2, updated_at = now()
WHERE id = $1 AND status IN ('creating', 'create_uncertain')
RETURNING ` + receiptColumns

// MarkReady records the provider resource id of a created clone. Only a
// creating or uncertain receipt may become ready, once.
func (s *ReceiptStore) MarkReady(ctx context.Context, id int64, ref string) (Receipt,
	error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > maxReceiptText {
		return Receipt{}, fmt.Errorf("%w: ref must be 1-%d characters", ErrInvalidReceipt,
			maxReceiptText)
	}
	if s.pool == nil {
		return Receipt{}, ErrNoReceiptStore
	}
	got, err := scanReceipt(s.pool.QueryRow(ctx, markReadySQL, id, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, s.stateError(ctx, id)
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("mark clone receipt %d ready: %w", id, err)
	}
	return got, nil
}

// stateError tells a missing receipt from one in the wrong state.
func (s *ReceiptStore) stateError(ctx context.Context, id int64) error {
	var status string
	err := s.pool.QueryRow(ctx, `/* pg_sage clone_receipt v1 */
		SELECT status FROM sage.clone_instances WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: id %d", ErrReceiptNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("read clone receipt %d: %w", id, err)
	}
	return fmt.Errorf("%w: receipt %d is %s", ErrReceiptState, id, status)
}

const activeReceiptSQL = `/* pg_sage clone_receipt v1 */
SELECT ` + receiptColumns + ` FROM sage.clone_instances
WHERE name = $1 AND status = 'ready' AND expires_at > now()
ORDER BY created_at DESC, id DESC LIMIT 1`

// Active returns the newest ready, unexpired receipt for the clone
// registered under name (a sandbox is monitored under its receipt name).
func (s *ReceiptStore) Active(ctx context.Context, name string) (Receipt, bool, error) {
	if s.pool == nil {
		return Receipt{}, false, ErrNoReceiptStore
	}
	if strings.TrimSpace(name) == "" {
		return Receipt{}, false, nil
	}
	r, err := scanReceipt(s.pool.QueryRow(ctx, activeReceiptSQL, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read clone receipt for %s: %w", name, err)
	}
	return r, true, nil
}
