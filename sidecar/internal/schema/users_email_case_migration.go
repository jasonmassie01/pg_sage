package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Case-insensitive user emails (apiauth-05). sage.users.email was a
// case-sensitive UNIQUE, so Alice@corp.com and alice@corp.com could be two
// accounts. Once no two emails differ only in case or surrounding spaces,
// the migration rewrites them trimmed and in lower case and adds the
// users_email_lower unique index. When duplicates exist it changes
// nothing: it logs an ERROR naming the colliding user ids (never the
// emails) and opens a critical finding, because which account survives is
// the operator's decision. Sign-in with such an email fails closed, the
// bootstrap succeeds, and the next bootstrap after the operator resolves
// the duplicates completes the migration and resolves the finding.

// usersEmailCaseFinding is the finding's category; object_identifier is
// sage.users.
const usersEmailCaseFinding = "auth_email_case_duplicates"

// usersEmailMigrationTimeout bounds the whole step; the lock wait is
// bounded separately by lock_timeout.
const usersEmailMigrationTimeout = 30 * time.Second

const usersEmailIndexExistsSQL = `/* pg_sage users_email_case v1 */
SELECT to_regclass('sage.users_email_lower') IS NOT NULL`

const usersEmailDuplicatesSQL = `/* pg_sage users_email_case v1 */
SELECT COALESCE(jsonb_agg(ids ORDER BY ids[1]), '[]'::jsonb)::text
  FROM (SELECT array_agg(id ORDER BY id) AS ids FROM sage.users
         GROUP BY lower(btrim(email)) HAVING count(*) > 1) d`

const usersEmailRewriteSQL = `/* pg_sage users_email_case v1 */
UPDATE sage.users SET email = lower(btrim(email))
 WHERE email <> lower(btrim(email))`

const usersEmailIndexSQL = `/* pg_sage users_email_case v1 */
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower ON sage.users (lower(email))`

const usersEmailRaiseFindingSQL = `/* pg_sage users_email_case v1 */
INSERT INTO sage.findings AS f (category, severity, object_type, object_identifier,
    title, detail, recommendation, status, last_seen, occurrence_count)
VALUES ($1, 'critical', 'table', 'sage.users',
    'User accounts whose emails differ only in case block case-insensitive sign-in',
    $2::jsonb, $3, 'open', now(), 1)
ON CONFLICT (category, object_identifier) WHERE status = 'open' DO UPDATE SET
    detail = EXCLUDED.detail, last_seen = now(),
    occurrence_count = f.occurrence_count + 1`

const usersEmailResolveFindingSQL = `/* pg_sage users_email_case v1 */
UPDATE sage.findings SET status = 'resolved', resolved_at = now()
 WHERE category = $1 AND object_identifier = 'sage.users' AND status = 'open'`

const usersEmailRecommendation = "Each group of user ids shares one email " +
	"that differs only in case. Sign-in with those emails is refused until " +
	"one account per email remains: delete or rename the others in sage.users " +
	"(re-grant roles and links to the account you keep), then restart pg_sage " +
	"to finish the migration."

// migrateUsersEmailCase applies the case-insensitive email migration (see
// above). A lock that cannot be had in time defers it to the next
// bootstrap.
func migrateUsersEmailCase(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, usersEmailMigrationTimeout)
	defer cancel()
	var done bool
	if err := db.QueryRow(qctx, usersEmailIndexExistsSQL).Scan(&done); err != nil {
		return fmt.Errorf("users email case migration: checking index: %w", err)
	}
	if done {
		return nil
	}
	dups, err := lowerUsersEmails(qctx, db)
	if locksCleanupDeferrable(err) {
		slog.Warn("users email case migration deferred to the next startup: "+
			"sage.users is busy", "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("users email case migration: %w", err)
	}
	if len(dups) == 0 {
		if _, err := db.Exec(qctx, usersEmailResolveFindingSQL,
			usersEmailCaseFinding); err != nil {
			return fmt.Errorf("users email case migration: resolving finding: %w", err)
		}
		return nil
	}
	slog.Error("sage.users has accounts whose emails differ only in case; "+
		"case-insensitive sign-in is not enforced and those emails cannot sign in "+
		"until an operator keeps one account per email",
		"duplicate_user_ids", fmt.Sprint(dups), "finding", usersEmailCaseFinding)
	return raiseUsersEmailFinding(qctx, db, dups)
}

// lowerUsersEmails returns the case-variant duplicate groups, or, when
// there are none, rewrites the emails and creates the index. The table is
// locked against writes so no duplicate appears between check and index.
func lowerUsersEmails(ctx context.Context, db bootstrapDB) ([][]int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s';
		LOCK TABLE sage.users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return nil, fmt.Errorf("locking sage.users: %w", err)
	}
	dups, err := usersEmailDuplicates(ctx, tx)
	if err != nil || len(dups) > 0 {
		return dups, err
	}
	if _, err := tx.Exec(ctx, usersEmailRewriteSQL); err != nil {
		return nil, fmt.Errorf("lower-casing emails: %w", err)
	}
	if _, err := tx.Exec(ctx, usersEmailIndexSQL); err != nil {
		return nil, fmt.Errorf("creating users_email_lower: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return nil, nil
}

func usersEmailDuplicates(ctx context.Context, tx pgx.Tx) ([][]int, error) {
	var raw string
	if err := tx.QueryRow(ctx, usersEmailDuplicatesSQL).Scan(&raw); err != nil {
		return nil, fmt.Errorf("finding case-variant emails: %w", err)
	}
	var dups [][]int
	if err := json.Unmarshal([]byte(raw), &dups); err != nil {
		return nil, fmt.Errorf("decoding case-variant emails: %w", err)
	}
	return dups, nil
}

func raiseUsersEmailFinding(ctx context.Context, db bootstrapDB, dups [][]int) error {
	detail, err := json.Marshal(map[string]any{"duplicate_user_ids": dups})
	if err != nil {
		return fmt.Errorf("users email case migration: encoding finding: %w", err)
	}
	if _, err := db.Exec(ctx, usersEmailRaiseFindingSQL, usersEmailCaseFinding,
		string(detail), usersEmailRecommendation); err != nil {
		return fmt.Errorf("users email case migration: raising finding: %w", err)
	}
	return nil
}
