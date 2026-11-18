package plan

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Model authority is the flag review-05a's G-M1 sets once a person
// approves it: a live sage.review_model_authority row for a normalized
// model id. With it, that model's steps run at their class's trust level
// on a plan approval; without it (or after a revocation), each needs its
// own approval. A grant names who granted it; nothing here decides one.

// dbtx is a pool, a connection or a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GrantModelAuthority records that grantedBy (a person) granted model
// authority, with why. A re-grant after a revocation makes the row live
// again.
func GrantModelAuthority(ctx context.Context, db dbtx, model, grantedBy, reason string) error {
	m := modellift.NormalizeModel(model)
	by := strings.TrimSpace(grantedBy)
	if m == "" || by == "" {
		return fmt.Errorf("%w: a model authority grant names the model and who granted it "+
			"(model %q, by %q)", ErrInvalidInput, model, grantedBy)
	}
	_, err := db.Exec(ctx, `/* pg_sage */ INSERT INTO sage.review_model_authority
		(model, granted_by, reason) VALUES ($1, $2, $3)
		ON CONFLICT (model) DO UPDATE SET granted_by = EXCLUDED.granted_by,
		granted_at = now(), revoked_at = NULL, reason = EXCLUDED.reason`, m, clip(by, 200),
		clip(reason, 1000))
	if err != nil {
		return fmt.Errorf("grant model authority to %s: write "+
			"sage.review_model_authority: %w", m, err)
	}
	return nil
}

// RevokeModelAuthority drops model's authority at once (a failed G-M1
// window or a model change), with why. Revoking a model without a live
// grant changes nothing.
func RevokeModelAuthority(ctx context.Context, db dbtx, model, reason string) error {
	m := modellift.NormalizeModel(model)
	if m == "" {
		return fmt.Errorf("%w: no model to revoke", ErrInvalidInput)
	}
	_, err := db.Exec(ctx, `/* pg_sage */ UPDATE sage.review_model_authority
		SET revoked_at = now(), reason = $2 WHERE model = $1 AND revoked_at IS NULL`, m,
		clip(reason, 1000))
	if err != nil {
		return fmt.Errorf("revoke model authority of %s: write "+
			"sage.review_model_authority: %w", m, err)
	}
	return nil
}

// ModelAuthority reports whether model holds a live grant.
func ModelAuthority(ctx context.Context, db dbtx, model string) (bool, error) {
	m := modellift.NormalizeModel(model)
	if m == "" {
		return false, nil
	}
	var live bool
	err := db.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
		FROM sage.review_model_authority WHERE model = $1 AND revoked_at IS NULL)`, m).
		Scan(&live)
	if err != nil {
		return false, fmt.Errorf("read the model authority of %s: %w", m, err)
	}
	return live, nil
}
