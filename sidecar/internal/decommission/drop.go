package decommission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DropPath is the admin-only route that drops the legacy tables (§12 step 5).
const DropPath = InventoryPath + "/drop-legacy-tables"

// DropConfirmPhrase is the exact phrase a drop request must carry.
var DropConfirmPhrase = fmt.Sprintf("drop %d legacy tables", len(LegacyTables))

var (
	// ErrUnacknowledged refuses the drop while any inventory item is
	// unacknowledged: the tables are the only record of those resources.
	ErrUnacknowledged = errors.New("inventory items are unacknowledged: delete or keep " +
		"each one, acknowledge it (POST " + AckPath + "), then drop")
	// ErrStaleInventory refuses a digest that is not the current inventory's.
	ErrStaleInventory = errors.New("inventory_sha256 is stale: the inventory changed " +
		"since it was exported; export it again (GET " + InventoryPath + ") and review it")
	// ErrBadConfirm refuses a request without the exact confirmation phrase.
	ErrBadConfirm = errors.New("confirm must be exactly " + fmt.Sprintf("%q", DropConfirmPhrase))
	// ErrBadDigest refuses a request whose digest is not lowercase hex SHA-256.
	ErrBadDigest = errors.New("inventory_sha256 must be the 64 lowercase hex characters " +
		"the exported inventory carries")
	// ErrNoActor refuses a drop that names no admin.
	ErrNoActor = errors.New("a legacy table drop needs an actor")
	// ErrDropBusy means a lock was not granted within the lock timeout.
	ErrDropBusy = errors.New("the legacy tables are in use or another drop is running; " +
		"retry shortly")
	// ErrAuditTableMissing refuses a drop that could not be audited.
	ErrAuditTableMissing = errors.New("sage.config_audit does not exist: start pg_sage " +
		"once so its schema bootstrap creates it")
)

// DependencyError means another object depends on a legacy table, so the
// whole drop was refused and nothing was dropped.
type DependencyError struct{ Message, Detail string }

func (e *DependencyError) Error() string {
	return "the legacy tables were not dropped: " + e.Message + ": " +
		strings.ReplaceAll(e.Detail, "\n", "; ") +
		". Drop or move the dependent objects, then retry"
}

// DropRequest is the body of POST DropPath.
type DropRequest struct {
	InventorySHA256 string `json:"inventory_sha256"`
	Confirm         string `json:"confirm"`
}

// DropResult lists the legacy tables the drop removed, in LegacyTables order.
type DropResult struct {
	Dropped []string `json:"dropped"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// dropLockKey serializes drops so a second one sees the first one's result.
const dropLockKey = "pg_sage decommission drop legacy tables"

// DropLegacyTables drops the legacy tables that still exist, in one
// transaction: only when every inventory item is acknowledged and the caller
// reviewed the current inventory. It never cascades, so a dependent object
// refuses the whole drop. Each dropped table gets a sage.config_audit row.
func DropLegacyTables(ctx context.Context, pool *pgxpool.Pool, req DropRequest,
	actor string) (DropResult, error) {
	if pool == nil {
		return DropResult{}, errors.New("drop the legacy tables: no control database pool")
	}
	if err := validateDrop(req, actor); err != nil {
		return DropResult{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return DropResult{}, fmt.Errorf("drop the legacy tables: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	dropped, err := dropInTx(ctx, tx, req, actor)
	if err != nil {
		return DropResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DropResult{}, fmt.Errorf("drop the legacy tables: commit: %w", err)
	}
	return DropResult{Dropped: dropped}, nil
}

func validateDrop(req DropRequest, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return ErrNoActor
	}
	if req.Confirm != DropConfirmPhrase {
		return ErrBadConfirm
	}
	if !sha256Hex.MatchString(req.InventorySHA256) {
		return ErrBadDigest
	}
	return nil
}

func dropInTx(ctx context.Context, tx pgx.Tx, req DropRequest, actor string) (
	[]string, error) {
	if err := lockForDrop(ctx, tx); err != nil {
		return nil, err
	}
	present, err := presentLegacyTables(ctx, tx)
	if err != nil || len(present) == 0 {
		return present, err
	}
	if err := checkAuditTable(ctx, tx); err != nil {
		return nil, err
	}
	if err := lockTables(ctx, tx, present); err != nil {
		return nil, err
	}
	if err := checkInventory(ctx, tx, req); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "/* pg_sage agentdb_decommission v1 */ DROP TABLE "+
		qualified(present)); err != nil {
		return nil, classifyDropErr("drop", err)
	}
	if _, err := tx.Exec(ctx, `/* pg_sage agentdb_decommission v1 */
		INSERT INTO sage.config_audit (key, old_value, new_value, changed_by_actor)
		SELECT 'decommission.legacy_table.' || t, 'present', 'dropped', $2
		FROM unnest($1::text[]) WITH ORDINALITY AS u(t, n) ORDER BY n`,
		present, actor); err != nil {
		return nil, fmt.Errorf("drop the legacy tables: audit: %w", err)
	}
	return present, nil
}

// lockForDrop bounds every lock wait and takes the drop's advisory lock.
func lockForDrop(ctx context.Context, tx pgx.Tx) error {
	for _, stmt := range []string{"SET LOCAL lock_timeout = '5s'",
		"SET LOCAL statement_timeout = '60s'"} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("drop the legacy tables: %s: %w", stmt, err)
		}
	}
	if _, err := tx.Exec(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, dropLockKey); err != nil {
		return classifyDropErr("lock", err)
	}
	return nil
}

// presentLegacyTables lists the legacy tables that exist, in LegacyTables order.
func presentLegacyTables(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT u.t FROM unnest($1::text[]) WITH ORDINALITY AS u(t, n)
		WHERE EXISTS (SELECT 1 FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
			WHERE ns.nspname = 'sage' AND c.relname = u.t AND c.relkind IN ('r', 'p'))
		ORDER BY u.n`, LegacyTables)
	if err != nil {
		return nil, fmt.Errorf("drop the legacy tables: look them up: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("drop the legacy tables: look them up: %w", err)
	}
	return append([]string{}, names...), nil
}

func checkAuditTable(ctx context.Context, tx pgx.Tx) error {
	var exists bool
	if err := tx.QueryRow(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT to_regclass('sage.config_audit') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("drop the legacy tables: look up sage.config_audit: %w", err)
	}
	if !exists {
		return ErrAuditTableMissing
	}
	return nil
}

// lockTables holds the tables so no row the inventory reads changes before
// the drop.
func lockTables(ctx context.Context, tx pgx.Tx, names []string) error {
	if _, err := tx.Exec(ctx, "/* pg_sage agentdb_decommission v1 */ LOCK TABLE "+
		qualified(names)+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return classifyDropErr("lock", err)
	}
	return nil
}

// checkInventory rebuilds the inventory inside the transaction and refuses
// while an item is unacknowledged or the caller's digest is stale.
func checkInventory(ctx context.Context, tx pgx.Tx, req DropRequest) error {
	st, err := load(ctx, tx)
	if err != nil {
		return fmt.Errorf("drop the legacy tables: inventory: %w", err)
	}
	inv := buildInventory(st, os.LookupEnv, os.Environ(), time.Now())
	if inv.Unacknowledged > 0 {
		return fmt.Errorf("%d %w", inv.Unacknowledged, ErrUnacknowledged)
	}
	if inv.InventorySHA256 != req.InventorySHA256 {
		return ErrStaleInventory
	}
	return nil
}

func qualified(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = pgx.Identifier{"sage", n}.Sanitize()
	}
	return strings.Join(parts, ", ")
}

// classifyDropErr maps a dependency (2BP01) and a lock timeout (55P03) to
// their typed errors.
func classifyDropErr(step string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "2BP01":
			return &DependencyError{Message: pgErr.Message, Detail: pgErr.Detail}
		case "55P03":
			return fmt.Errorf("%w: %s", ErrDropBusy, pgErr.Message)
		}
	}
	return fmt.Errorf("drop the legacy tables: %s: %w", step, err)
}
