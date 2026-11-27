//go:build cgo

package broker

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// A column the agent may not read (a secret anywhere, PII in prod) is an
// oracle when it is used outside the output: a filter on it answers
// whether a guessed value exists, one bit per call. Found by security-02's
// agent EXPLAIN tests; agent_query and explain_query share this check.
// No concurrent access tests: the check is per call and holds no state.

func TestQueryRefusesDeniedColumnOutsideOutput(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	for _, sql := range []string{"SELECT id FROM people WHERE ssn = '123-45-6789'",
		"SELECT id FROM people ORDER BY ssn", "SELECT id FROM people WHERE ssn " +
			"LIKE '123%'"} {
		res := f.query(t, sql)
		require.Equal(t, VerdictBlocked, res.Verdict, "%s: %+v", sql, res)
		require.Equal(t, string(decide.ReasonClassification), res.ReasonCode, sql)
		require.Empty(t, res.Rows, "%s: rows came back for a denied predicate", sql)
		require.Contains(t, res.Detail, "ssn", sql)
	}
	// Clean columns of the same relation stay usable everywhere.
	res := f.query(t, "SELECT id FROM people WHERE id > 0 ORDER BY id")
	require.Equal(t, StatusOK, res.Status, "%+v", res)
	require.Len(t, res.Rows, 2)
}

func TestQueryRefusesSecretColumnInFilterInDev(t *testing.T) {
	f := newFixture(t, envbind.EnvDev)
	f.classifyColumn(t, "people", "api_key", classify.ClassSecret)
	res := f.query(t, "SELECT id FROM people WHERE api_key IS NOT NULL")
	require.Equal(t, VerdictBlocked, res.Verdict, "%+v", res)
	require.Equal(t, string(decide.ReasonClassification), res.ReasonCode)
	require.Contains(t, res.Detail, "api_key")
	audit := f.auditRows(t)
	require.Len(t, audit, 1)
	require.Equal(t, string(decide.ReasonClassification), audit[0].reason)
}
