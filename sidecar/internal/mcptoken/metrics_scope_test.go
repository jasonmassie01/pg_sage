package mcptoken_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// observability-08: the metrics scope authorizes a Prometheus scrape of
// /metrics and nothing else. It is a token's only scope, on an operator
// token (created for an operator or admin; metrics is read-level, so a
// viewer owner keeps it), so a scrape credential never reads or proposes
// through MCP. Pure validation; no Postgres.

// No concurrent access tests here: validation is stateless.

func metricsOperator() mcptoken.CreateRequest {
	req := validOperator()
	req.Scopes = []string{mcptoken.ScopeMetrics}
	req.Databases = []string{"*"}
	return req
}

var metricsScopeCases = []invalidCase{
	{"metrics with read", metricsOperator,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"metrics", "read"} },
		mcptoken.ErrInvalid, "metrics"},
	{"metrics with propose and read", metricsOperator,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"read", "propose", "metrics"} },
		mcptoken.ErrInvalid, "metrics"},
	{"metrics on an agent token", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"metrics"} },
		mcptoken.ErrInvalid, "operator"},
	{"metrics without owner", metricsOperator,
		func(r *mcptoken.CreateRequest) { r.OwnerUserID = 0 }, mcptoken.ErrOwnerRequired, ""},
	{"upper-case metrics", metricsOperator,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"METRICS"} },
		mcptoken.ErrInvalid, "scope"},
	// /metrics is the whole sidecar's exposition and cannot be narrowed to
	// a database, so a database-scoped scrape token would promise a limit
	// it cannot keep.
	{"metrics for one database", metricsOperator,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"orders"} },
		mcptoken.ErrInvalid, "databases"},
	{"metrics for two databases", metricsOperator,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"orders", "billing"} },
		mcptoken.ErrInvalid, "databases"},
}

func TestCreateRejectsInvalidMetricsScopes(t *testing.T) { runInvalid(t, metricsScopeCases) }

func TestCreateMetricsOnlyOperatorTokenPassesValidation(t *testing.T) {
	requireReachesStorage(t, metricsOperator())
	dup := metricsOperator()
	dup.Scopes = []string{"metrics", "metrics"}
	requireReachesStorage(t, dup)
}

func TestMetricsScopeConstant(t *testing.T) {
	require.Equal(t, "metrics", mcptoken.ScopeMetrics)
}

// Nil/empty: without a pool, ValidateMetrics refuses a malformed secret as
// unauthorized and a well-formed one with a storage error, never a grant.
func TestValidateMetricsWithoutPool(t *testing.T) {
	s := mcptoken.NewStore(nil)
	for _, secret := range []string{"", "Bearer", "pgs_mcp_short"} {
		g, err := s.ValidateMetrics(context.Background(), secret)
		require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
		require.Equal(t, "", g.TokenID)
	}
	wellFormed := mcptoken.SecretPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	g, err := s.ValidateMetrics(context.Background(), wellFormed)
	require.Error(t, err)
	require.False(t, mcptokenRefusal(err), "a missing pool is a storage error: %v", err)
	require.Equal(t, "", g.TokenID)
	var nilStore *mcptoken.Store
	_, err = nilStore.ValidateMetrics(context.Background(), wellFormed)
	require.Error(t, err)
}

func mcptokenRefusal(err error) bool {
	for _, sentinel := range []error{mcptoken.ErrUnauthorized, mcptoken.ErrScopeMissing} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
