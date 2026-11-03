package api

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// The Findings explorer's Rules filter must retain the plan-independent spill
// finding and attribute it to Rules, rather than showing an empty Source cell.
func TestTempSpillVisibleInRulesSource(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	ident := fmt.Sprintf("queryid:temp_spill_source_%d", time.Now().UnixNano())
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation, status)
		VALUES ('temp_spill', 'warning', 'query', $1, 'Observed temporary blocks',
		 '{"query":"SELECT payload FROM public.orders ORDER BY payload", "delta_temp_blks_written":256}',
		 'Investigate the spill in a controlled session.', 'open') RETURNING id`, ident).Scan(&id)
	if err != nil {
		t.Fatalf("seed spill finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE id=$1", id)
	})

	h := findingsListHandler(phase2MgrWithPool(pool))
	rows, status := getList(t, h, "/api/v1/findings?database=testdb&source=rules&category=temp_spill&limit=1000")
	if status != 200 {
		t.Fatalf("Rules findings status=%d", status)
	}
	found := false
	for _, row := range rows.Findings {
		if row["id"] != strconv.FormatInt(id, 10) {
			continue
		}
		found = true
		if row["category"] != "temp_spill" || row["subsystem"] != "rules" {
			t.Fatalf("spill attribution=%v, want temp_spill from rules", row)
		}
	}
	if !found {
		t.Fatal("Rules source filter hid the new temp_spill finding")
	}

	other, status := getList(t, h, "/api/v1/findings?database=testdb&source=advisor&category=temp_spill&limit=1000")
	if status != 200 {
		t.Fatalf("Advisor findings status=%d", status)
	}
	for _, row := range other.Findings {
		if row["id"] == strconv.FormatInt(id, 10) {
			t.Fatal("Rules temp_spill finding leaked into Advisor source filter")
		}
	}
}
