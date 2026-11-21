package agenttools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No state transition tests: FindCallSite is stateless per call; it keeps
// nothing between lookups.

const csRepo = "acme/shop"

// rubyLines is a file of n numbered lines with a trailing newline.
func rubyLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "  # line %d\n", i)
	}
	return b.String()
}

func fileLineTags(path, line string) map[string]string {
	return map[string]string{"file": path, "line": line}
}

func TestFindCallSiteFileAndLineTagIsExact(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "app/models/order.rb", rubyLines(50))
	got, err := FindCallSite(context.Background(), host, csRepo, 77,
		fileLineTags("app/models/order.rb", "42"), "")
	require.NoError(t, err)
	require.Equal(t, CallSiteExact, got.Confidence)
	require.Equal(t, "app/models/order.rb", got.Path)
	require.Equal(t, 42, got.Line)
	require.Equal(t, CallSiteMethodFileLine, got.Method)
	require.Equal(t, QueryID(77), got.QueryID)
	require.Empty(t, got.Candidates)
	require.Equal(t, "Called from app/models/order.rb:42 (exact)", got.Summary())
	require.Equal(t, 1, host.Calls(), "one GetFile, no search")
}

func TestFindCallSiteRejectsInvalidTagsBeforeAnyRequest(t *testing.T) {
	cases := map[string]map[string]string{
		"line zero":         fileLineTags("app/models/order.rb", "0"),
		"negative line":     fileLineTags("app/models/order.rb", "-3"),
		"line not number":   fileLineTags("app/models/order.rb", "forty"),
		"line huge":         fileLineTags("app/models/order.rb", "99999999999"),
		"dot dot path":      fileLineTags("app/../../etc/passwd", "1"),
		"leading dot dot":   fileLineTags("../secrets.rb", "1"),
		"absolute path":     fileLineTags("/srv/app/models/order.rb", "42"),
		"windows absolute":  fileLineTags(`C:\app\order.rb`, "42"),
		"file without line": {"file": "../order.rb"},
	}
	for name, tags := range cases {
		t.Run(name, func(t *testing.T) {
			host := codehosttest.New()
			host.AddFile(csRepo, "app/models/order.rb", rubyLines(50))
			_, err := FindCallSite(context.Background(), host, csRepo, 1, tags,
				"SELECT 1 FROM orders")
			require.ErrorIs(t, err, ErrInvalid)
			require.Equal(t, 0, host.Calls(), "no request before validation")
		})
	}
}

func TestFindCallSiteRejectsBadRepoAndMissingHost(t *testing.T) {
	host := codehosttest.New()
	_, err := FindCallSite(context.Background(), host, "../acme", 1, nil, "SELECT 1")
	require.ErrorIs(t, err, ErrInvalid)
	_, err = FindCallSite(context.Background(), host, "", 1, nil, "SELECT 1")
	require.ErrorIs(t, err, ErrInvalid)
	require.Equal(t, 0, host.Calls())
	_, err = FindCallSite(context.Background(), nil, csRepo, 1, nil, "SELECT 1")
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorContains(t, err, "no code host")
}

func TestFindCallSiteNoTagsNoLiteralIsNone(t *testing.T) {
	host := codehosttest.New()
	for _, sql := range []string{"", "   ", "/* only a comment */", "SELECT 1"} {
		got, err := FindCallSite(context.Background(), host, csRepo, 9, nil, sql)
		require.NoError(t, err, sql)
		require.Equal(t, CallSiteNone, got.Confidence, sql)
		require.Equal(t, CallSiteMethodNone, got.Method, sql)
		require.Empty(t, got.Path, sql)
		require.Zero(t, got.Line, sql)
		require.Contains(t, got.Note, "add sqlcommenter tags", sql)
		require.True(t, strings.HasPrefix(got.Summary(), "No call site found"), got.Summary())
	}
	require.Equal(t, 0, host.Calls(), "nothing to search for sends nothing")
}

func TestFindCallSiteLineBoundary(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "app/models/order.rb", rubyLines(50))
	host.AddFile(csRepo, "app/models/no_newline.rb", strings.TrimSuffix(rubyLines(7), "\n"))
	at, err := FindCallSite(context.Background(), host, csRepo, 1,
		fileLineTags("app/models/order.rb", "50"), "")
	require.NoError(t, err)
	require.Equal(t, CallSiteExact, at.Confidence)
	require.Equal(t, 50, at.Line)
	past, err := FindCallSite(context.Background(), host, csRepo, 1,
		fileLineTags("app/models/order.rb", "51"), "")
	require.NoError(t, err)
	require.Equal(t, CallSiteNone, past.Confidence)
	require.Empty(t, past.Path)
	require.Contains(t, past.Note, "past the end of app/models/order.rb (50 lines)")
	last, err := FindCallSite(context.Background(), host, csRepo, 1,
		fileLineTags("app/models/no_newline.rb", "7"), "")
	require.NoError(t, err)
	require.Equal(t, CallSiteExact, last.Confidence, "last line without a newline counts")
	one, err := FindCallSite(context.Background(), host, csRepo, 1,
		fileLineTags("app/models/order.rb", "1"), "")
	require.NoError(t, err)
	require.Equal(t, 1, one.Line)
}

func TestFindCallSiteLineWithoutFileIsNotARule(t *testing.T) {
	host := codehosttest.New()
	got, err := FindCallSite(context.Background(), host, csRepo, 1,
		map[string]string{"line": "42"}, "")
	require.NoError(t, err)
	require.Equal(t, CallSiteNone, got.Confidence)
	require.Equal(t, 0, host.Calls())
}

func TestFindCallSiteMissingTaggedFileFallsThrough(t *testing.T) {
	host := codehosttest.New()
	got, err := FindCallSite(context.Background(), host, csRepo, 1,
		fileLineTags("app/models/gone.rb", "3"), "")
	require.NoError(t, err)
	require.Equal(t, CallSiteNone, got.Confidence)
	require.Contains(t, got.Note, "app/models/gone.rb is not in acme/shop")
}

const ordersController = `class OrdersController < ApplicationController
  def show
    @order = Order.find(params[:id])
  end

  def index
    @orders = Order.open
  end
end
`

const adminOrdersController = `module Admin
  class OrdersController < BaseController
    def index
      @orders = Order.all
    end
  end
end
`

func TestFindCallSiteControllerTagsTwoMatchesAreLikelyCandidates(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "app/controllers/orders_controller.rb", ordersController)
	host.AddFile(csRepo, "app/controllers/admin/orders_controller.rb", adminOrdersController)
	host.AddFile(csRepo, "README.md", "orders are listed here")
	tags := map[string]string{"controller": "Orders", "action": "index"}
	got, err := FindCallSite(context.Background(), host, csRepo, 1, tags, "")
	require.NoError(t, err)
	require.Equal(t, CallSiteLikely, got.Confidence)
	require.Equal(t, CallSiteMethodRoute, got.Method)
	require.Empty(t, got.Path, "several candidates are never guessed")
	require.Zero(t, got.Line)
	require.Equal(t, []CallSiteCandidate{
		{Path: "app/controllers/admin/orders_controller.rb", Line: 3},
		{Path: "app/controllers/orders_controller.rb", Line: 6},
	}, got.Candidates)
	require.Contains(t, got.Summary(), "one of 2 places (likely)")
}

func TestFindCallSiteOneRouteMatchIsLikelyNeverExact(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "app/controllers/orders_controller.rb", ordersController)
	tags := map[string]string{"controller": "Orders", "action": "show"}
	got, err := FindCallSite(context.Background(), host, csRepo, 1, tags, "")
	require.NoError(t, err)
	require.Equal(t, CallSiteLikely, got.Confidence, "a search hit is not proof")
	require.Equal(t, "app/controllers/orders_controller.rb", got.Path)
	require.Equal(t, 2, got.Line)
	require.Len(t, got.Candidates, 1)
}

const goRepoFile = `package store

func (s *Store) OpenOrders(ctx context.Context, customer int64) error {
	_, err := s.db.Query(ctx,
		"SELECT id, total FROM orders WHERE customer_id = $1 AND status = 'open'",
		customer)
	return err
}

func (s *Store) Count(ctx context.Context) error {
	_, err := s.db.Query(ctx, "SELECT count(*) FROM customers")
	return err
}
`

const pgssOpenOrders = "SELECT id, total FROM orders WHERE customer_id = $1 AND " +
	"status = $2 /*application='shop'*/"

func TestFindCallSiteSQLLiteralFingerprintIsExact(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "internal/store/orders.go", goRepoFile)
	host.AddFile(csRepo, "internal/store/other.go", "package store // orders customer_id")
	got, err := FindCallSite(context.Background(), host, csRepo, 5, nil, pgssOpenOrders)
	require.NoError(t, err)
	require.Equal(t, CallSiteExact, got.Confidence)
	require.Equal(t, CallSiteMethodLiteral, got.Method)
	require.Equal(t, "internal/store/orders.go", got.Path)
	require.Equal(t, 5, got.Line)
}

func TestFindCallSiteSQLLiteralTwiceIsLikely(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "a/orders.go", goRepoFile)
	host.AddFile(csRepo, "b/orders.rb", "def open\n  sql = <<~SQL\n    select id, total\n"+
		"    from orders where customer_id = ? and status = ?\n  SQL\nend\n")
	got, err := FindCallSite(context.Background(), host, csRepo, 5, nil, pgssOpenOrders)
	require.NoError(t, err)
	require.Equal(t, CallSiteLikely, got.Confidence)
	require.Empty(t, got.Path)
	require.Equal(t, []CallSiteCandidate{{Path: "a/orders.go", Line: 5},
		{Path: "b/orders.rb", Line: 2}}, got.Candidates)
}

func TestFindCallSiteSQLLiteralOfAnotherStatementIsNone(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "internal/store/orders.go", goRepoFile)
	sql := "SELECT id, total FROM orders WHERE customer_id = $1 AND region = $2"
	got, err := FindCallSite(context.Background(), host, csRepo, 5, nil, sql)
	require.NoError(t, err)
	require.Equal(t, CallSiteNone, got.Confidence)
	require.Contains(t, got.Note, "add sqlcommenter tags")
}

func TestFindCallSiteExactLiteralBeatsLikelyRoute(t *testing.T) {
	host := codehosttest.New()
	host.AddFile(csRepo, "app/controllers/orders_controller.rb", ordersController)
	host.AddFile(csRepo, "internal/store/orders.go", goRepoFile)
	tags := map[string]string{"controller": "Orders", "action": "index"}
	got, err := FindCallSite(context.Background(), host, csRepo, 5, tags, pgssOpenOrders)
	require.NoError(t, err)
	require.Equal(t, CallSiteExact, got.Confidence)
	require.Equal(t, "internal/store/orders.go", got.Path)
}

func TestFindCallSiteHostErrorsPropagate(t *testing.T) {
	host := codehosttest.New()
	host.Fail(fmt.Errorf("%w: token cannot read acme/shop", codehost.ErrForbidden))
	_, err := FindCallSite(context.Background(), host, csRepo, 1,
		fileLineTags("app/models/order.rb", "4"), "")
	require.ErrorIs(t, err, codehost.ErrForbidden)
	require.ErrorContains(t, err, "token cannot read acme/shop")
	require.ErrorContains(t, err, "query 1")
	host.Fail(&codehost.RateLimitError{Host: "fake", RetryAfter: 30e9})
	_, err = FindCallSite(context.Background(), host, csRepo, 1, nil, pgssOpenOrders)
	var rl *codehost.RateLimitError
	require.True(t, errors.As(err, &rl), "rate limit stays distinguishable: %v", err)
	require.ErrorIs(t, err, codehost.ErrRateLimited)
}

func TestSQLFingerprintKeepsIdentifiersAndDropsValues(t *testing.T) {
	a := sqlFingerprint("SELECT * FROM Orders WHERE id=$1 AND s IN ($2, $3) /*x='y'*/;")
	b := sqlFingerprint("select *\n  from orders\n where id = ? and s in (?)")
	c := sqlFingerprint(`select * from orders where id = :id and s in (%s)`)
	d := sqlFingerprint("SELECT * FROM customers WHERE id = $1 AND s IN ($2)")
	require.Equal(t, a, b)
	require.Equal(t, a, c)
	require.NotEqual(t, a, d, "a different table is a different statement")
	require.Equal(t, sqlFingerprint("SELECT a::int FROM t"),
		sqlFingerprint("select a :: int from t"), "a cast is not a named parameter")
	require.Equal(t, "", sqlFingerprint("  /* c */ -- line\n"))
}
