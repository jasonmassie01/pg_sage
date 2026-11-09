package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/notify"
)

// CountPlaintextSecrets counts stored secrets that are not sealed: config
// secret rows and notification channels with a plaintext secret
// (security-08). It reads only and is bounded by a short timeout.
func CountPlaintextSecrets(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(qctx, `/* pg_sage secrets_posture v1 */ SELECT
		(SELECT count(*) FROM sage.config
		  WHERE key = ANY($1) AND value <> '' AND value NOT LIKE 'sage-enc:v1:%')
		+ (SELECT count(*) FROM sage.notification_channels c
		  WHERE EXISTS (SELECT 1 FROM jsonb_each_text(CASE
		      WHEN jsonb_typeof(c.config) = 'object' THEN c.config
		      ELSE '{}'::jsonb END) e
		    WHERE e.key = ANY($2) AND e.value <> ''
		      AND e.value NOT LIKE 'sage-enc:v1:%' AND e.value NOT LIKE 'enc:v1:%'))`,
		secretConfigKeys(), notify.SecretConfigKeys).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting plaintext secrets: %w", err)
	}
	return n, nil
}
