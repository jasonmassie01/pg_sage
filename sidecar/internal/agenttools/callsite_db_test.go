package agenttools

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests here: CallSite only reads pg_stat_statements;
// the concurrency cap is tested over the GitHub client in
// callsite_host_test.go.

func TestCallSiteFromTaggedPgStatStatementsText(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	orders := f.ordersTable(200)
	tagged := "SELECT count(*) FROM " + orders + " WHERE status = $1 " +
		"/*file='app%2Fmodels%2Forder.rb',line='42',controller='orders'*/"
	literal := "SELECT max(amount) FROM " + orders + " WHERE status = $1"
	host := codehosttest.New()
	host.AddFile(csRepo, "app/models/order.rb", rubyLines(50))
	host.AddFile(csRepo, "internal/report.go", "package report\n\nconst q = `\n"+
		strings.ReplaceAll(literal, "$1", "'open'")+"`\n")
	tools := New(f.pool, Options{Excluded: testExcluded})
	pgssepoch.Attempt(t, f.ctx, f.pool, 3, func() []string {
		f.resetOwnStatements()
		f.execArgs(tagged, "open")
		f.execArgs(literal, "open")
		tagID, ok1 := f.statementID("count(*) FROM " + orders)
		litID, ok2 := f.statementID("max(amount) FROM " + orders)
		if !ok1 || !ok2 {
			return []string{"fixture statements missing from pg_stat_statements"}
		}
		got, err := tools.CallSite(f.ctx, host, csRepo, QueryID(tagID))
		require.NoError(t, err)
		require.Equal(t, CallSiteExact, got.Confidence, "tags read from pgss text")
		require.Equal(t, "app/models/order.rb", got.Path)
		require.Equal(t, 42, got.Line)
		require.Equal(t, QueryID(tagID), got.QueryID)
		got, err = tools.CallSite(f.ctx, host, csRepo, QueryID(litID))
		require.NoError(t, err)
		require.Equal(t, CallSiteExact, got.Confidence, "literal matched: %+v", got)
		require.Equal(t, CallSiteMethodLiteral, got.Method)
		require.Equal(t, "internal/report.go", got.Path)
		require.Equal(t, 3, got.Line)
		return nil
	})
}

func TestCallSiteUnknownQueryIDIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	host := codehosttest.New()
	_, err := New(f.pool, Options{}).CallSite(f.ctx, host, csRepo, QueryID(-4242424242))
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, fmt.Sprint(-4242424242))
	require.Equal(t, 0, host.Calls(), "no code-host request for an unknown statement")
	_, err = New(f.pool, Options{}).CallSite(f.ctx, host, csRepo, 0)
	require.True(t, errors.Is(err, ErrInvalid), "query id 0: %v", err)
}
