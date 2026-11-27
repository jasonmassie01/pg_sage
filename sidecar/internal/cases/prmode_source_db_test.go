package cases_test

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func noContract(string) (string, string) { return "", "" }

// The loader returns an open finding with a change once, keeps returning
// it while its PR is followed, and drops it once the PR is declined.
func TestPRModeDB_LoadCandidatesFollowsThePRLifecycle(t *testing.T) {
	pool := prModePool(t)
	ctx := context.Background()
	id := migrationFinding(t, pool)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
	    object_identifier, title, detail, status, recommended_sql) VALUES ('x', 'info',
	    'table', 't', 'resolved one', '{}', 'resolved', 'ALTER TABLE t ADD COLUMN c int')`)
	require.NoError(t, err)
	got, err := cases.LoadPRCandidates(ctx, pool, noContract, 10)
	require.NoError(t, err)
	require.Equal(t, 1, len(got), "only the open finding with a change")
	require.Equal(t, id, got[0].FindingID)
	require.Equal(t, cases.PRKindMigration, got[0].Kind)

	host := dbHost()
	m := dbPRMode(t, pool, host)
	_, err = m.Run(ctx, got[0])
	require.NoError(t, err)
	again, err := cases.LoadPRCandidates(ctx, pool, noContract, 10)
	require.NoError(t, err)
	require.Equal(t, 1, len(again), "an open PR is still followed")
	host.SetState(dbRepo, codehost.PRStatus{Number: 1, State: codehost.PRClosed})
	_, err = m.Follow(ctx, again[0])
	require.NoError(t, err)
	done, err := cases.LoadPRCandidates(ctx, pool, noContract, 10)
	require.NoError(t, err)
	require.Empty(t, done, "a declined PR is not followed or proposed again")
	none, err := cases.LoadPRCandidates(ctx, pool, noContract, 0)
	require.NoError(t, err)
	require.Empty(t, none, "limit 0 reads nothing")
}
