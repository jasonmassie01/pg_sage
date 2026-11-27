package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func getOldValue(
	ctx context.Context, tx pgx.Tx,
	key string, databaseID int,
) (string, error) {
	var old string
	var err error
	if databaseID == 0 {
		err = tx.QueryRow(ctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`, key,
		).Scan(&old)
	} else {
		err = tx.QueryRow(ctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id = $2`, key, databaseID,
		).Scan(&old)
	}
	if err != nil {
		return "", nil // no existing value
	}
	return old, nil
}

func upsertOverride(
	ctx context.Context, tx pgx.Tx,
	key, value string, databaseID int, userID int,
) error {
	var updatedBy *int // 0 is a system change: no user to name
	if userID > 0 {
		updatedBy = &userID
	}
	if databaseID == 0 {
		_, err := tx.Exec(ctx,
			`/* pg_sage */ INSERT INTO sage.config
				(key, value, database_id, updated_at, updated_by_user_id)
			 VALUES ($1, $2, NULL, now(), $3)
			 ON CONFLICT (key, COALESCE(database_id, 0))
			 DO UPDATE SET value = $2, updated_at = now(),
				updated_by_user_id = $3`,
			key, value, updatedBy)
		return err
	}
	_, err := tx.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config
			(key, value, database_id, updated_at, updated_by_user_id)
		 VALUES ($1, $2, $3, now(), $4)
		 ON CONFLICT (key, COALESCE(database_id, 0))
		 DO UPDATE SET value = $2, updated_at = now(),
			updated_by_user_id = $4`,
		key, value, databaseID, updatedBy)
	return err
}

func insertAudit(
	ctx context.Context, tx pgx.Tx,
	key, oldValue, newValue string,
	databaseID int, userID int,
) error {
	oldValue, newValue = auditValues(key, oldValue, newValue)
	var dbID *int
	if databaseID > 0 {
		dbID = &databaseID
	}
	var chBy *int
	if userID > 0 {
		chBy = &userID
	}
	// changed_by_actor snapshots the caller's email (apiauth-01): it is
	// sealed with the row, so the trail names the person after their
	// account is deleted. An id with no users row here (fleet mode keeps
	// users on the auth database) leaves it NULL.
	_, err := tx.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config_audit
			(key, old_value, new_value, database_id, changed_by, changed_by_actor)
		 VALUES ($1, $2, $3, $4, $5::int,
			(SELECT u.email FROM sage.users u WHERE u.id = $5::int))`,
		key, nullIfEmpty(oldValue), newValue, dbID, chBy)
	return err
}

const redactedSecret = "[REDACTED]"

// secretKeys are the config keys whose values are credentials: redacted in
// audit and API reads, and sealed at rest when encryption_key is set.
var secretKeys = []string{
	"llm.api_key",
	"alerting.slack_webhook_url",
	"alerting.pagerduty_routing_key",
	"briefing.slack_webhook_url",
	"clone.dle_token",
}

// IsSecretConfigKey reports a key whose value is a credential (secretKeys).
func IsSecretConfigKey(key string) bool {
	for _, k := range secretKeys {
		if k == key {
			return true
		}
	}
	return false
}

func secretConfigKeys() []string {
	return append([]string(nil), secretKeys...)
}

func auditValues(key, oldValue, newValue string) (string, string) {
	if !IsSecretConfigKey(key) {
		return oldValue, newValue
	}
	if oldValue != "" {
		oldValue = redactedSecret
	}
	if newValue != "" {
		newValue = redactedSecret
	}
	return oldValue, newValue
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanOverrideRows(
	rows pgx.Rows,
) ([]ConfigOverride, error) {
	var results []ConfigOverride
	for rows.Next() {
		var o ConfigOverride
		err := rows.Scan(
			&o.Key, &o.Value, &o.DatabaseID,
			&o.UpdatedAt, &o.UpdatedBy)
		if err != nil {
			return nil, fmt.Errorf("scanning override: %w", err)
		}
		results = append(results, o)
	}
	return results, rows.Err()
}

func scanAuditRows(rows pgx.Rows) ([]ConfigAuditEntry, error) {
	var results []ConfigAuditEntry
	for rows.Next() {
		var e ConfigAuditEntry
		err := rows.Scan(
			&e.ID, &e.Key, &e.OldValue, &e.NewValue,
			&e.DatabaseID, &e.ChangedBy, &e.ChangedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning audit: %w", err)
		}
		e.OldValue, e.NewValue = auditValues(
			e.Key, e.OldValue, e.NewValue,
		)
		results = append(results, e)
	}
	return results, rows.Err()
}

func maskSecret(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}
