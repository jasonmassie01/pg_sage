package value

import (
	"testing"
)

// Regression test for SURF-03 / G2-B11: an action credited without a
// database attribution must not make the whole value report fail.
func TestFleetReadCountsUnattributedCreditUnderInstanceName(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, toil_minutes_saved, toil_model_version)
		VALUES ('analyze_table', 'SELECT 1', 'success', 15, 1) RETURNING id`).
		Scan(&id); err != nil {
		t.Fatalf("insert unattributed credit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", id)
	})

	report, err := fleetOf(Source{Name: "primary", Pool: pool}).
		Get(ctx, Filter{Database: "primary"})

	if err != nil || report.Partial {
		t.Fatalf("read with NULL database_id: partial=%v err=%v", report.Partial, err)
	}
	if report.DBAHoursSaved.AllTime < 0.25 {
		t.Fatalf("unattributed credit missing from report: %#v", report)
	}
	// T4: standalone rows carry no database_id, yet filtering by the
	// configured instance name must count them under that name.
	if len(report.ByDatabase) != 1 || report.ByDatabase[0].Name != "primary" {
		t.Fatalf("unattributed credit not labelled by instance: %#v", report.ByDatabase)
	}
}
