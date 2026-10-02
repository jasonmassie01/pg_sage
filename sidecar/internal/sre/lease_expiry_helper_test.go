package sre

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// expireLease ends a lease now, as if its worker died and the TTL ran
// out, instead of sleeping past a tiny TTL and racing the host's load.
// The whole lease segment moves back by one amount (lease_started_at,
// lease_until and segment_deadline), so the time an orphaned lease is
// charged on the next claim (lease_until - lease_started_at) is exactly
// the TTL that was granted. now() is fixed for the statement, so all
// three columns shift identically; the extra millisecond makes the
// lease strictly expired for every later clock_timestamp() check.
func expireLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, l Lease) {
	t.Helper()
	tag, err := pool.Exec(ctx, `UPDATE sage.sre_investigations
		SET lease_started_at = lease_started_at
		        - (lease_until - now() + interval '1 millisecond'),
		    segment_deadline = segment_deadline
		        - (lease_until - now() + interval '1 millisecond'),
		    lease_until = now() - interval '1 millisecond'
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3
		  AND lease_owner = $4 AND fence_token = $5`, leaseArgs(l)...)
	if err != nil {
		t.Fatalf("expire lease of %s (fence %d): %v", l.InvestigationID, l.Fence, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expire lease of %s (fence %d): %d rows, want the held lease",
			l.InvestigationID, l.Fence, tag.RowsAffected())
	}
}
