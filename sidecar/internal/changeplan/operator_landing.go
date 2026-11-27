package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LandingCheck is one read of an operator step's landing: whether it
// landed, when (the read, or the managed change's applied_at) and what was
// read, for the step's reason.
type LandingCheck struct {
	Landed bool
	At     time.Time
	Detail string
}

// LandingReader reads operator steps' landings and their managed changes'
// rollback commands (DBLandings in production).
type LandingReader interface {
	CheckLanding(ctx context.Context, s Step, now time.Time) (LandingCheck, error)
	RevertCommand(ctx context.Context, s Step) (string, error)
}

// DBLandings reads landings on the monitored database: pg_settings and
// sage.managed_change_proposals, read-only and bounded.
type DBLandings struct{ pool *pgxpool.Pool }

// NewDBLandings reads landings on pool.
func NewDBLandings(pool *pgxpool.Pool) DBLandings { return DBLandings{pool: pool} }

// CheckLanding implements LandingReader: every check the step declares
// must hold (its setting reads the expected value with no restart
// pending; its managed change is applied). It lands at the later of the
// two: the managed change's applied_at, or now for the setting.
func (d DBLandings) CheckLanding(ctx context.Context, s Step, now time.Time) (LandingCheck,
	error) {
	if s.Landing == nil && s.ManagedChangeID == nil {
		return LandingCheck{}, errors.New("the step has no landing check")
	}
	var parts []string
	landed, at := true, time.Time{}
	err := d.readOnly(ctx, func(tx pgx.Tx) error {
		if s.ManagedChangeID != nil {
			ok, applied, detail, err := readManaged(ctx, tx, *s.ManagedChangeID)
			if err != nil {
				return err
			}
			landed, at = landed && ok, applied
			parts = append(parts, detail)
		}
		if s.Landing != nil {
			ok, detail, err := readLandingSetting(ctx, tx, *s.Landing)
			if err != nil {
				return err
			}
			landed, at = landed && ok, now
			parts = append(parts, detail)
		}
		return nil
	})
	if err != nil {
		return LandingCheck{}, err
	}
	return LandingCheck{Landed: landed, At: at, Detail: strings.Join(parts, "; ")}, nil
}

// readOnly runs fn in a read-only transaction bounded by lookupTimeout.
// It sets no SET LOCAL timeouts as the store's transactions do: a landing
// on statement_timeout or lock_timeout must read the server's value, not
// the reader's own.
func (d DBLandings) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin the landing read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func readManaged(ctx context.Context, tx pgx.Tx, id int64) (bool, time.Time, string, error) {
	var status string
	var applied *time.Time
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT status, applied_at
		FROM sage.managed_change_proposals WHERE id = $1`, id).Scan(&status, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, "", fmt.Errorf("managed change %d is gone", id)
	}
	if err != nil {
		return false, time.Time{}, "", fmt.Errorf("read managed change %d: %w", id, err)
	}
	if status != "applied" || applied == nil {
		return false, time.Time{}, fmt.Sprintf("managed change %d is %s", id, status), nil
	}
	return true, *applied, fmt.Sprintf("managed change %d is applied", id), nil
}

func readLandingSetting(ctx context.Context, tx pgx.Tx, l Landing) (bool, string, error) {
	var setting, display string
	var pending bool
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT setting, current_setting(name),
		pending_restart FROM pg_catalog.pg_settings WHERE name = $1`, l.Setting).Scan(
		&setting, &display, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", fmt.Errorf("setting %s is not in pg_settings", l.Setting)
	}
	if err != nil {
		return false, "", fmt.Errorf("read setting %s: %w", l.Setting, err)
	}
	ok, detail := landingRead(l, setting, display, pending)
	return ok, detail, nil
}

// landingRead is whether landing l holds on what pg_settings read: the
// setting reads l.Value with no restart pending; detail says what was
// read.
func landingRead(l Landing, setting, display string, pending bool) (bool, string) {
	if pending {
		return false, fmt.Sprintf("%s is %s with a restart pending (want %s)", l.Setting,
			display, l.Value)
	}
	if !settingMatches(l.Value, setting, display) {
		return false, fmt.Sprintf("%s is %s (want %s)", l.Setting, display, l.Value)
	}
	return true, fmt.Sprintf("%s is %s", l.Setting, display)
}

// RevertCommand implements LandingReader: the managed change's rollback
// CLI ("" when the step has no managed change or it gives none).
func (d DBLandings) RevertCommand(ctx context.Context, s Step) (string, error) {
	if s.ManagedChangeID == nil {
		return "", nil
	}
	var cmd string
	err := (&Store{pool: d.pool}).inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(proposal #>> '{rollback,cli}',
			'') FROM sage.managed_change_proposals WHERE id = $1`,
			*s.ManagedChangeID).Scan(&cmd)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the rollback of managed change %d: %w",
			*s.ManagedChangeID, err)
	}
	return cmd, nil
}

// settingMatches reports whether want is the setting's value as
// pg_settings stores it or as SHOW prints it (case and quotes aside), so
// "64MB" matches a work_mem of 65536 kB.
func settingMatches(want, setting, display string) bool {
	w := strings.ToLower(strings.Trim(strings.TrimSpace(want), `'"`))
	if w == "" {
		return false
	}
	return w == strings.ToLower(setting) || w == strings.ToLower(display)
}
