package optimizer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// market-15b review: fillCatalog must fill LeadingKeyStats on PostgreSQL
// 18, or the skip-scan rule almost never sees the composite's leading-key
// n_distinct (ColStats holds only workload-named columns). Before 18 the
// extra pg_stats read must not happen at all.

// leadingStatsQuerier answers the collation-and-version read with major
// and the named-columns pg_stats read with one tenant_id row; every
// other read fails like failingQuerier.
type leadingStatsQuerier struct {
	versionQuerier
	mu    sync.Mutex
	asked [][]string
}

func (q *leadingStatsQuerier) Query(_ context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	if !strings.Contains(sql, "attname = ANY($3)") || len(args) != 3 {
		return nil, q.failingQuerier.err
	}
	cols, _ := args[2].([]string)
	q.mu.Lock()
	q.asked = append(q.asked, cols)
	q.mu.Unlock()
	return &statRows{rows: []ColStat{{Column: "tenant_id", NDistinct: 12}}}, nil
}

// statRows is a pgx.Rows over pg_stats rows (attname, n_distinct, NULL
// correlation and MCV arrays).
type statRows struct {
	rows []ColStat
	next int
}

func (r *statRows) Close()                                       {}
func (r *statRows) Err() error                                   { return nil }
func (r *statRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *statRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *statRows) Values() ([]any, error)                       { return nil, nil }
func (r *statRows) RawValues() [][]byte                          { return nil }
func (r *statRows) Conn() *pgx.Conn                              { return nil }

func (r *statRows) Next() bool {
	r.next++
	return r.next <= len(r.rows)
}

func (r *statRows) Scan(dest ...any) error {
	if len(dest) != 5 {
		return errors.New("want attname, n_distinct, correlation, mcv and mcf")
	}
	s := r.rows[r.next-1]
	*dest[0].(*string), *dest[1].(*float64) = s.Column, s.NDistinct
	return nil
}

func leadingStatsTable(major int) TableContext {
	return TableContext{Schema: "public", Table: "t", Indexes: []IndexInfo{{
		Name: "t_tenant_created", IsValid: true,
		Definition: "CREATE INDEX t_tenant_created ON public.t USING btree " +
			"(tenant_id, created_at)"}}, PGMajor: major}
}

func TestFillCatalog_ReadsLeadingKeyStatsOnPG18(t *testing.T) {
	q := &leadingStatsQuerier{versionQuerier: versionQuerier{
		failingQuerier: failingQuerier{errStatementTimeout}, major: 18}}
	o := New(nil, admissionConfig(), noopLog2)
	tc := leadingStatsTable(0)
	o.fillCatalog(context.Background(), q, &tc, nil)
	if tc.PGMajor != 18 {
		t.Fatalf("PGMajor = %d, want 18", tc.PGMajor)
	}
	want := []ColStat{{Column: "tenant_id", NDistinct: 12}}
	if !reflect.DeepEqual(tc.LeadingKeyStats, want) {
		t.Fatalf("LeadingKeyStats = %+v, want %+v", tc.LeadingKeyStats, want)
	}
	if !reflect.DeepEqual(q.asked, [][]string{{"tenant_id"}}) {
		t.Fatalf("named pg_stats reads = %q, want one for tenant_id", q.asked)
	}
	ix, ok := skipScanCandidate(tc, candidate(createdAtDDL))
	if !ok || ix.Name != "t_tenant_created" {
		t.Fatalf("skipScanCandidate = %q %t after fillCatalog, want t_tenant_created",
			ix.Name, ok)
	}
}

func TestFillCatalog_NoLeadingKeyReadBeforePG18(t *testing.T) {
	q := &leadingStatsQuerier{versionQuerier: versionQuerier{
		failingQuerier: failingQuerier{errStatementTimeout}, major: 17}}
	o := New(nil, admissionConfig(), noopLog2)
	tc := leadingStatsTable(0)
	o.fillCatalog(context.Background(), q, &tc, nil)
	if tc.PGMajor != 17 || tc.LeadingKeyStats != nil || len(q.asked) != 0 {
		t.Fatalf("PG 17: PGMajor %d, LeadingKeyStats %+v, named reads %q; want 17, "+
			"none and none", tc.PGMajor, tc.LeadingKeyStats, q.asked)
	}
}

// State transition: once the serving composite is dropped, the agent's
// packet no longer tells the model the idea is served by it (admission
// already judges the candidate again); other remembered lines stay.
func TestSkipScan_PromptLineGoneOnceTheServingIndexIsDropped(t *testing.T) {
	store, plans := newMemStore(), servedPlans()
	o, _, _ := skipOptimizer(store, plans, verifiedResult)
	if a := o.Admit(context.Background(), candidate(createdAtDDL), skipTable()); a.Outcome !=
		AdmitRejected {
		t.Fatalf("first cycle = %s %q", a.Outcome, a.Reason)
	}
	if lines := o.MeasuredRejections(context.Background(), skipTable()); len(lines) != 1 {
		t.Fatalf("prompt lines while served = %q, want one", lines)
	}
	dropped := skipTable()
	dropped.Indexes, dropped.IndexCount = nil, 0
	if lines := o.MeasuredRejections(context.Background(), dropped); len(lines) != 0 {
		t.Fatalf("prompt lines after the drop = %q, want none", lines)
	}
	unknown := skipTable()
	unknown.PGMajor = 0
	if lines := o.MeasuredRejections(context.Background(), unknown); len(lines) != 0 {
		t.Fatalf("prompt lines with the version unknown = %q, want none", lines)
	}
}
