package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// market-15b: reading plan evidence, the agent's PostgreSQL 18 note and
// the server version the skip-scan rule depends on.

// No concurrency tests: the version is read per table context with the
// collation, so there is no shared state to race on.

func TestPlanServedBy(t *testing.T) {
	bitmap := `[{"Plan": {"Node Type": "Bitmap Heap Scan", "Plans": [{"Node Type":
	  "Bitmap Index Scan", "Index Name": "t_tenant_created",
	  "Index Cond": "((created_at >= $1) AND (created_at < $2))"}]}}]`
	quoted := `[{"Plan": {"Node Type": "Index Only Scan", "Index Name": "t_tenant_created",
	  "Index Cond": "(\"CreatedAt\" = $1)"}}]`
	longer := strings.Replace(skipScanPlanJSON, "(created_at = $1)",
		"(created_at_old = $1)", 1)
	cases := []struct {
		name, plan, index, col string
		want                   bool
	}{
		{"index scan", skipScanPlanJSON, skipIndexName, "created_at", true},
		{"nested bitmap", bitmap, skipIndexName, "created_at", true},
		{"quoted column", quoted, skipIndexName, "CreatedAt", true},
		{"quoted column, other case", quoted, skipIndexName, "createdat", false},
		{"longer column name", longer, skipIndexName, "created_at", false},
		{"other index", skipScanPlanJSON, "t_other", "created_at", false},
		{"seq scan", seqScanPlanJSON, skipIndexName, "created_at", false},
		{"not json", "not json", skipIndexName, "created_at", false},
		{"empty", "", skipIndexName, "created_at", false},
	}
	for _, c := range cases {
		if got := planServedBy([]byte(c.plan), c.index, c.col); got != c.want {
			t.Errorf("%s: planServedBy = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestSkipScanNote(t *testing.T) {
	tc := skipTable()
	if note := SkipScanNote(tc); !strings.Contains(note, "skip scan") ||
		!strings.Contains(note, "PostgreSQL 18") {
		t.Fatalf("PG 18 note = %q", note)
	}
	for _, major := range []int{0, 14, 17} {
		tc.PGMajor = major
		if note := SkipScanNote(tc); note != "" {
			t.Errorf("PG %d note = %q, want none", major, note)
		}
	}
}

// versionQuerier answers the collation-and-version read; every other
// read fails like failingQuerier.
type versionQuerier struct {
	failingQuerier
	major int
	err   error
}

func (q versionQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if !strings.Contains(sql, "server_version_num") {
		return errRow{q.failingQuerier.err}
	}
	return collationRow{major: q.major, err: q.err}
}

type collationRow struct {
	major int
	err   error
}

func (r collationRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 4 {
		return errors.New("want datcollate, provider, locale and the major version")
	}
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*string) = "en_US.utf8", "c", ""
	*dest[3].(*int) = r.major
	return nil
}

// The context carries the major version read with the collation (no
// extra round trip).
func TestFillCatalog_ReadsTheMajorVersionWithTheCollation(t *testing.T) {
	o := New(nil, admissionConfig(), noopLog2)
	tc := TableContext{Schema: "public", Table: "t"}
	o.fillCatalog(context.Background(), versionQuerier{
		failingQuerier: failingQuerier{errStatementTimeout}, major: 18}, &tc, nil)
	if tc.PGMajor != 18 || tc.Collation != "en_US.utf8" {
		t.Fatalf("PGMajor %d collation %q, want 18 and en_US.utf8", tc.PGMajor, tc.Collation)
	}
}

// Error propagation: a failed read is an unknown version (0: no
// version-specific rule applies), logged with the table and the error.
func TestFillCatalog_VersionReadErrorIsUnknownAndLogged(t *testing.T) {
	logs := &logRecorder{}
	o := New(nil, admissionConfig(), logs.log)
	tc := TableContext{Schema: "public", Table: "t", PGMajor: 18}
	o.fillCatalog(context.Background(), versionQuerier{
		failingQuerier: failingQuerier{errStatementTimeout}, err: errStatementTimeout},
		&tc, nil)
	if tc.PGMajor != 0 || tc.Collation != "" {
		t.Fatalf("PGMajor %d collation %q after a failed read, want 0 and none",
			tc.PGMajor, tc.Collation)
	}
	got := logs.matching("server version", "statement timeout", "public.t", "skip scan")
	if len(got) != 1 {
		t.Fatalf("logs = %q, want one line naming the table and the error", logs.lines)
	}
}

// fillCatalog reads the leading-key statistics only on PostgreSQL 18:
// before it the context is unchanged.
func TestLeadingKeyColumns(t *testing.T) {
	tc := TableContext{PGMajor: 18, Indexes: []IndexInfo{
		{Name: "a", IsValid: true, Definition: "CREATE INDEX a ON public.t USING btree " +
			"(tenant_id, created_at)"},
		{Name: "b", IsValid: true, Definition: "CREATE INDEX b ON public.t USING btree " +
			"(region, created_at)"},
		{Name: "c", IsValid: true, Definition: "CREATE INDEX c ON public.t USING btree (id)"},
		{Name: "d", IsValid: true, Definition: "CREATE INDEX d ON public.t USING gin " +
			"(tags, kind)"},
		{Name: "e", IsValid: false, Definition: "CREATE INDEX e ON public.t USING btree " +
			"(shard, id)"},
	}, ColStats: []ColStat{{Column: "region"}}}
	if got := strings.Join(leadingKeyColumns(tc), ","); got != "tenant_id" {
		t.Fatalf("leading key columns = %q, want tenant_id only", got)
	}
	tc.PGMajor = 17
	if got := leadingKeyColumns(tc); got != nil {
		t.Fatalf("PG 17 leading key columns = %q, want none", got)
	}
}

// A string literal in the Index Cond that spells the column's name is not
// a condition on the column: dropping a candidate needs a real one.
func TestPlanServedBy_IgnoresStringLiterals(t *testing.T) {
	literal := `[{"Plan": {"Node Type": "Index Scan", "Index Name": "t_tenant_created",
	  "Index Cond": "(tenant_id = 'created_at'::text)"}}]`
	escaped := `[{"Plan": {"Node Type": "Index Scan", "Index Name": "t_tenant_created",
	  "Index Cond": "(tenant_id = 'it''s created_at'::text)"}}]`
	both := `[{"Plan": {"Node Type": "Index Scan", "Index Name": "t_tenant_created",
	  "Index Cond": "((tenant_id = 'x'::text) AND (created_at = $1))"}}]`
	cases := []struct {
		name, plan string
		want       bool
	}{
		{"literal only", literal, false},
		{"escaped quote in literal", escaped, false},
		{"literal and a real condition", both, true},
	}
	for _, c := range cases {
		got := planServedBy([]byte(c.plan), "t_tenant_created", "created_at")
		if got != c.want {
			t.Errorf("%s: planServedBy = %t, want %t", c.name, got, c.want)
		}
	}
}
