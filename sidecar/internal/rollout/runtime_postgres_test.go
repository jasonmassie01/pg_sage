package rollout

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestPostgresRunStoreRoundTripsSageRolloutRun(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SKIPPED: SAGE_TEST_DATABASE_URL is not configured")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store := NewPostgresRunStore(pool)
	record := RunRecord{
		EvidenceID:      "rollout-runtime-contract",
		SourceInstance:  "source-a",
		PriorEvidenceID: "prior-a",
		Policy: Policy{
			CanaryInstances: 2, MaxAffectedInstances: 5,
			AggregateRegressionLimitPct: 10,
			RequireLocalReverification:  true,
		},
		State: "canary", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.rollout_run WHERE evidence_id=$1", record.EvidenceID)
	})

	require.NoError(t, store.Create(t.Context(), record))
	got, err := store.Get(t.Context(), record.EvidenceID)
	require.NoError(t, err)
	require.Equal(t, record.EvidenceID, got.EvidenceID)
	require.Equal(t, "canary", got.State)
	require.Equal(t, record.Policy, got.Policy)

	record.State = "halted"
	record.HaltReason = "aggregate_regression"
	record.AppliedInstances = 2
	record.AggregateRegressionPct = 17.5
	require.NoError(t, store.Update(t.Context(), record))
	got, err = store.Get(t.Context(), record.EvidenceID)
	require.NoError(t, err)
	require.Equal(t, "halted", got.State)
	require.Equal(t, 2, got.AppliedInstances)
	require.Equal(t, float64(17.5), got.AggregateRegressionPct)
}
