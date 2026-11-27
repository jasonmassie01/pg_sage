package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sre-08: sage.notification_log records which incident a delivery belongs
// to and the provider's message id, and sage.sre_incident_followup holds
// at most one diagnosis follow-up claim per incident.

func requireFollowUpShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var cols, idxDef string
	if err := pool.QueryRow(ctx, `SELECT string_agg(attname || ':' ||
		    format_type(atttypid, atttypmod) || ':' || (NOT attnotnull)::text, ','
		    ORDER BY attname)
		FROM pg_attribute WHERE attrelid = 'sage.notification_log'::regclass
		  AND attname IN ('ref_id', 'provider_message_id') AND NOT attisdropped`).
		Scan(&cols); err != nil {
		t.Fatalf("notification_log columns: %v", err)
	}
	if cols != "provider_message_id:text:true,ref_id:text:true" {
		t.Fatalf("notification_log columns = %q, want two nullable text columns", cols)
	}
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_indexdef('sage.idx_notification_log_ref'::regclass)`).
		Scan(&idxDef); err != nil || !strings.Contains(idxDef, "(ref_id)") ||
		!strings.Contains(idxDef, "ref_id IS NOT NULL") {
		t.Fatalf("ref index = %q (%v), want a partial index on ref_id", idxDef, err)
	}
	inc := "sre08-schema-incident"
	defer func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.sre_incident_followup WHERE incident_id = $1`, inc)
	}()
	claim := `INSERT INTO sage.sre_incident_followup (incident_id, investigation_id)
		VALUES ($1, gen_random_uuid()) ON CONFLICT DO NOTHING`
	for i, want := range []int64{1, 0} {
		tag, err := pool.Exec(ctx, claim, inc)
		if err != nil || tag.RowsAffected() != want {
			t.Fatalf("claim %d: %d rows (%v), want %d", i+1, tag.RowsAffected(), err, want)
		}
	}
	if _, err := pool.Exec(ctx, claim, ""); err == nil {
		t.Fatal("an empty incident id was accepted")
	}
	var claimedAt bool
	if err := pool.QueryRow(ctx, `SELECT claimed_at IS NOT NULL AND delivered_at IS NULL
		FROM sage.sre_incident_followup WHERE incident_id = $1`, inc).
		Scan(&claimedAt); err != nil || !claimedAt {
		t.Fatalf("a new claim must be timestamped and undelivered (%v)", err)
	}
}

// Integration: a fresh bootstrap and a second one end in the same shape.
func TestSREFollowUpMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireFollowUpShape(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS sage.idx_notification_log_ref;
		ALTER TABLE sage.notification_log DROP COLUMN IF EXISTS ref_id,
		    DROP COLUMN IF EXISTS provider_message_id`); err != nil {
		t.Fatalf("simulating the pre-sre-08 notification_log: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	requireFollowUpShape(t, ctx, pool)
}
