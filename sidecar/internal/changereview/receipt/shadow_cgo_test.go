//go:build cgo

package receipt

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// CHECK-RCP-08 eligibility fixtures: only allowlisted statement kinds,
// each parsed with libpg_query, make a safe review would-auto-clear.

func TestEligibleStatementKinds(t *testing.T) {
	cases := []struct {
		name   string
		sql    []string
		bytes  int64
		kind   string
		ok     bool
		reason string
	}{
		{"CIC on a 1 GB table", []string{cic}, gib, "table", true, ""},
		{"CIC over the size limit", []string{cic}, 11 * gib, "table", false, "over"},
		{"CIC at the size limit", []string{cic}, 10 * gib, "table", true, ""},
		{"index build without CONCURRENTLY", []string{
			"CREATE INDEX rv_orders_status_idx ON rv.rv_orders (status)"}, gib, "table",
			false, "CONCURRENTLY"},
		{"CIC on a partitioned table", []string{cic}, gib, "partitioned", false,
			"partitioned"},
		{"nullable ADD COLUMN", []string{"ALTER TABLE rv.rv_orders ADD COLUMN note text"},
			gib, "table", true, ""},
		{"ADD COLUMN with a stable default", []string{
			"ALTER TABLE rv.rv_orders ADD COLUMN seen_at timestamptz DEFAULT now()"}, gib,
			"table", true, ""},
		{"ADD COLUMN NOT NULL", []string{
			"ALTER TABLE rv.rv_orders ADD COLUMN flag boolean NOT NULL DEFAULT false"}, gib,
			"table", false, "NOT NULL"},
		{"ADD COLUMN with a volatile default", []string{
			"ALTER TABLE rv.rv_orders ADD COLUMN token uuid DEFAULT gen_random_uuid()"}, gib,
			"table", false, "volatile"},
		{"ADD COLUMN with a foreign key", []string{
			"ALTER TABLE rv.rv_orders ADD COLUMN item_id int REFERENCES rv.rv_items (id)"},
			gib, "table", false, "constraint"},
		{"CREATE TABLE", []string{"CREATE TABLE rv.new_t (id bigint PRIMARY KEY, at " +
			"timestamptz)"}, gib, "table", true, ""},
		{"CREATE TABLE with a foreign key", []string{"CREATE TABLE rv.new_t (id bigint " +
			"PRIMARY KEY, order_id bigint REFERENCES rv.rv_orders (id))"}, gib, "table",
			false, "foreign key"},
		{"CREATE TABLE PARTITION OF", []string{"CREATE TABLE rv.p1 PARTITION OF rv.p " +
			"FOR VALUES IN (1)"}, gib, "table", false, "partition"},
		{"CREATE STATISTICS", []string{"CREATE STATISTICS rv_s ON status, code FROM " +
			"rv.rv_orders"}, gib, "table", true, ""},
		{"COMMENT", []string{"COMMENT ON TABLE rv.rv_orders IS 'orders'"}, gib, "table",
			true, ""},
		{"SET then ADD COLUMN", []string{"SET lock_timeout = '2s'",
			"ALTER TABLE rv.rv_orders ADD COLUMN note text"}, gib, "table", true, ""},
		{"DROP COLUMN", []string{"ALTER TABLE rv.rv_orders DROP COLUMN status"}, gib,
			"table", false, "drop_column"},
		{"mixed ALTER TABLE", []string{"ALTER TABLE rv.rv_orders ADD COLUMN note text, " +
			"DROP COLUMN status"}, gib, "table", false, "drop_column"},
		{"unparseable", []string{"CREATE INDEX CONCURRENTLY ON"}, gib, "table", false,
			"parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := shadowReview(t, cic, tc.bytes, tc.kind)
			r.Statements = nil
			for i, sql := range tc.sql {
				r.Statements = append(r.Statements, changereview.Statement{Index: i + 1,
					Origin: changereview.OriginSQLInput, SQL: sql,
					Relations: []string{"rv.rv_orders"}})
			}
			ok, why := Eligible(r, 10*gib)
			if ok != tc.ok || (!tc.ok && !strings.Contains(why, tc.reason)) {
				t.Fatalf("Eligible = %v (%q), want %v naming %q", ok, why, tc.ok, tc.reason)
			}
			if tc.ok && why != "" {
				t.Fatalf("eligible with a reason %q", why)
			}
		})
	}
}

// A CIC whose table the review holds no evidence for is not eligible:
// unknown is never small.
func TestEligibleNeedsTheTableEvidence(t *testing.T) {
	r := shadowReview(t, cic, gib, "table")
	r.Evidence = nil
	if ok, why := Eligible(r, 10*gib); ok || !strings.Contains(why, "rv.rv_orders") {
		t.Fatalf("Eligible = %v (%q), want false naming the table", ok, why)
	}
	r = shadowReview(t, cic, gib, "table")
	r.Statements[0].Relations = nil
	if ok, _ := Eligible(r, 10*gib); ok {
		t.Fatal("a CIC with no resolved relation was eligible")
	}
}

func TestAutoClearEligibleCIC(t *testing.T) {
	r := shadowReview(t, cic, gib, "table")
	if ok, why := Eligible(r, 10*gib); !ok {
		t.Fatalf("a safe CIC on a 1 GB table is not eligible: %s", why)
	}
}
