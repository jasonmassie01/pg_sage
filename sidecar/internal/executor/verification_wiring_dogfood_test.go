package executor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// Dogfood lifeos-1 finding 5: "route custodian index through
// verification: index verification unavailable" read as if verification
// were not wired. It is wired for every executor; the refusal was a
// schema-guard index with no target queries. The error now says which
// precondition failed.

func TestNewExecutor_WiresIndexVerification(t *testing.T) {
	pool, _ := requireDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	if exec.indexVerification == nil {
		t.Fatal("a standalone executor has no index verification lifecycle")
	}
}

func TestVerifiedActionForFinding_NamesTheMissingPrecondition(t *testing.T) {
	base := analyzer.Finding{
		Category: "missing_fk_index", ObjectIdentifier: "public.orders",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders " +
			"(customer_id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY public.idx_orders_customer",
		Detail:      map[string]any{"table": "public.orders"},
	}
	cases := map[string]func(*analyzer.Finding){
		"target queries": func(*analyzer.Finding) {},
		"rollback":       func(f *analyzer.Finding) { f.RollbackSQL = "" },
		"index name":     func(f *analyzer.Finding) { f.RecommendedSQL = "CREATE INDEX ON t (a)" },
	}
	for want, mutate := range cases {
		f := base
		mutate(&f)
		_, err := verifiedActionForFinding(f)
		if !errors.Is(err, ErrVerificationUnavailable) || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want ErrVerificationUnavailable naming %q", err, want)
		}
	}
}
