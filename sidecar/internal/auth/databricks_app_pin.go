package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Identity pin modes (databricks-07), read from
// SAGE_DATABRICKS_APP_IDENTITY_PIN. An App account is keyed on the
// forwarded email, which the IdP may reassign to another person; the pin
// is the IdP user value the Apps proxy forwarded the first time the
// account was seen. Log records a changed value and lets the request
// through; enforce refuses it; off never reads or writes the pin.
const (
	DatabricksAppPinOff     = "off"
	DatabricksAppPinLog     = "log"
	DatabricksAppPinEnforce = "enforce"
)

const databricksAppPinEnv = "SAGE_DATABRICKS_APP_IDENTITY_PIN"

// pinPrefixLen is how much of a pin an audit row or log line carries:
// enough to tell two pins apart, never the whole stored value.
const pinPrefixLen = 12

// ErrDatabricksAppIdentityChanged means the forwarded IdP user value no
// longer matches the one pinned to this email's account (enforce mode).
var ErrDatabricksAppIdentityChanged = errors.New(
	"databricks forwarded user no longer matches the account's pinned identity")

func databricksAppPinMode(raw string) (string, error) {
	switch raw {
	case "":
		return DatabricksAppPinLog, nil
	case DatabricksAppPinOff, DatabricksAppPinLog, DatabricksAppPinEnforce:
		return raw, nil
	}
	return "", fmt.Errorf("%s must be off, log or enforce", databricksAppPinEnv)
}

// DatabricksAppUserPin is the stored form of a forwarded IdP user value:
// hex SHA-256 of the workspace, a NUL separator and the value.
func DatabricksAppUserPin(workspace, forwardedUser string) string {
	sum := sha256.Sum256([]byte(workspace + "\x00" + forwardedUser))
	return hex.EncodeToString(sum[:])
}

func (c DatabricksAppConfig) pinMode() string {
	if c.IdentityPin == "" {
		return DatabricksAppPinLog
	}
	return c.IdentityPin
}

// pinDB is the account store the pin check needs: a pool in production.
type pinDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// checkIdentityPin pins forwardedUser to u on first sight and compares it
// afterwards. An empty value, or off mode, leaves today's behavior.
func (c DatabricksAppConfig) checkIdentityPin(
	ctx context.Context, db pinDB, u *User, forwardedUser string,
) (*User, error) {
	mode := c.pinMode()
	if forwardedUser == "" || mode == DatabricksAppPinOff {
		return u, nil
	}
	seen := DatabricksAppUserPin(c.Workspace, forwardedUser)
	stored, err := loadOrClaimPin(ctx, db, u.ID, seen)
	if err != nil {
		return nil, err
	}
	if stored == seen {
		return u, nil
	}
	refused := mode == DatabricksAppPinEnforce
	recordPinMismatch(ctx, db, u.ID, stored, seen, mode, refused)
	if refused {
		return nil, ErrDatabricksAppIdentityChanged
	}
	return u, nil
}

// loadOrClaimPin returns the account's pin, first setting it to seen when
// none is stored. The claim only fills a NULL, so of two racing first
// sights one wins and the other reads the winner's pin back.
func loadOrClaimPin(ctx context.Context, db pinDB, userID int, seen string) (string, error) {
	for range 2 {
		var stored *string
		err := db.QueryRow(ctx, `SELECT databricks_user_pin FROM sage.users
			WHERE id = $1`, userID).Scan(&stored)
		if err != nil {
			return "", fmt.Errorf("reading Databricks identity pin of user %d: %w", userID, err)
		}
		if stored != nil {
			return *stored, nil
		}
		tag, err := db.Exec(ctx, `UPDATE sage.users SET databricks_user_pin = $2
			WHERE id = $1 AND databricks_user_pin IS NULL`, userID, seen)
		if err != nil {
			return "", fmt.Errorf("setting Databricks identity pin of user %d: %w", userID, err)
		}
		if tag.RowsAffected() == 1 {
			return seen, nil
		}
	}
	return "", fmt.Errorf("databricks identity pin of user %d changed during sign-in; retry",
		userID)
}

// recordPinMismatch logs and audits a changed identity at most once per
// user, stored and seen pin per pinAuditWindow, so a dashboard polling
// with a changed identity does not flood the audit chain; a failed write
// is retried on the next request. Detail carries the user id and pin
// prefixes, never an email or a forwarded value.
func recordPinMismatch(ctx context.Context, db pinDB, userID int,
	stored, seen, mode string, refused bool) {
	key := pinAuditKey{userID: userID, stored: stored, seen: seen}
	if !databricksPinAudits.allow(key) {
		return
	}
	storedPrefix, seenPrefix := stored[:min(len(stored), pinPrefixLen)], seen[:pinPrefixLen]
	slog.Warn("Databricks App forwarded user changed for a pinned account",
		"user_id", userID, "stored_pin_prefix", storedPrefix,
		"seen_pin_prefix", seenPrefix, "mode", mode, "refused", refused)
	err := RecordAuthAudit(context.WithoutCancel(ctx), db, AuthAuditEvent{
		Event: AuditDatabricksAppIdentityChanged, TargetUserID: userID,
		Detail: map[string]any{"user_id": userID, "stored_pin_prefix": storedPrefix,
			"seen_pin_prefix": seenPrefix, "mode": mode, "refused": refused},
	})
	if err != nil {
		// Not recorded: let the next request retry instead of losing the
		// event for a whole window.
		databricksPinAudits.release(key)
		slog.Error("auth audit write failed", "event", AuditDatabricksAppIdentityChanged,
			"target_user_id", userID, "error", err)
	}
}

// ResetDatabricksAppUserPin clears a user's pin so the next forwarded user
// value is pinned again (the relink an administrator performs after
// reviewing a changed identity). The audit row is written in the same
// transaction: a failed audit leaves the pin in place. It reports whether
// a pin was set, and ErrUserNotFound for an unknown user.
func ResetDatabricksAppUserPin(ctx context.Context, pool *pgxpool.Pool,
	actorID, userID int, sourceIP string) (bool, error) {
	if pool == nil {
		return false, fmt.Errorf("databricks account store is unavailable")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin Databricks identity pin reset: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var previous *string
	err = tx.QueryRow(ctx, `SELECT databricks_user_pin FROM sage.users
		WHERE id = $1 FOR UPDATE`, userID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, fmt.Errorf("reading Databricks identity pin of user %d: %w", userID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.users SET databricks_user_pin = NULL
		WHERE id = $1`, userID); err != nil {
		return false, fmt.Errorf("clearing Databricks identity pin of user %d: %w", userID, err)
	}
	var prefix any
	if previous != nil {
		prefix = (*previous)[:min(len(*previous), pinPrefixLen)]
	}
	if err := RecordAuthAudit(ctx, tx, AuthAuditEvent{
		Event: AuditDatabricksAppPinReset, ActorUserID: actorID, TargetUserID: userID,
		SourceIP: sourceIP,
		Detail:   map[string]any{"user_id": userID, "previous_pin_prefix": prefix},
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit Databricks identity pin reset: %w", err)
	}
	databricksPinAudits.forget(userID)
	return previous != nil, nil
}
