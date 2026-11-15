package querystore

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01b: a history migration between placements moves the daily tier
// and the coverage rows, rebuilds the hourly tier from the copied
// snapshots, and leaves the coverage report as it was, monitored -> meta
// and back.

type placed struct {
	daily  dailyRow
	hours  []map[int64]hourRow
	report CoverageReport
}

func readPlaced(t *testing.T, st histstore.Store, start, oldDay time.Time) placed {
	t.Helper()
	var out placed
	var ok bool
	if out.daily, ok = readDaily(t, st, 5, oldDay); !ok {
		t.Fatalf("no daily row for %s in the %s store", oldDay, st.Mode())
	}
	for k := range 3 {
		out.hours = append(out.hours, readHourRows(t, st, start.Add(time.Duration(k)*time.Hour)))
	}
	rep, err := RollupCoverage(context.Background(), st, start, start.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("coverage in the %s store: %v", st.Mode(), err)
	}
	rep.RollupBytes, rep.OldestHour = 0, nil // differ by placement, by design
	out.report = rep
	return out
}

func rebuildOption() histstore.MigrateOptions {
	return histstore.MigrateOptions{BatchRows: 7,
		Rebuild: func(ctx context.Context, dst histstore.Store) error {
			_, err := RebuildHourly(ctx, dst, 35)
			return err
		}}
}

func TestMigration_MovesTheRollupAndRebuildsTheHourlyTier(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	p.Switch(t, histstore.ModeMonitored)
	truncateAllRollup(t, p)
	start := writeThreeHours(t, p)
	for k := range 3 {
		if _, err := RollupHour(ctx, p.Monitored, start.Add(time.Duration(k)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	oldDay := pastDay(3)
	writeTwoDays(t, p.Monitored, oldDay)
	if _, err := RollupDay(ctx, p.Monitored, oldDay); err != nil {
		t.Fatal(err)
	}
	mon, meta := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	before := readPlaced(t, mon, start, oldDay)
	if before.report.CoveredExecShare == nil || before.report.Hours != 3 {
		t.Fatalf("fixture coverage %+v, want 3 known hours", before.report)
	}
	for _, leg := range []struct{ src, dst histstore.Store }{{mon, meta}, {meta, mon}} {
		rep, err := histstore.Migrate(ctx, leg.src, leg.dst, rebuildOption())
		if err != nil {
			t.Fatalf("migrate to %s: %v", leg.dst.Mode(), err)
		}
		if !rep.HourlyRebuilt {
			t.Fatalf("migrate to %s: the report must say the hourly rollup was rebuilt",
				leg.dst.Mode())
		}
		for _, table := range []string{"query_rollup_daily", "query_rollup_coverage"} {
			if tr := reportTable(rep, table); tr == nil || !tr.Complete || tr.Copied == 0 {
				t.Fatalf("migrate to %s: %s report %+v", leg.dst.Mode(), table, tr)
			}
		}
		if _, err := histstore.Cleanup(ctx, leg.src, leg.dst); err != nil {
			t.Fatalf("clean up after the move to %s: %v", leg.dst.Mode(), err)
		}
		after := readPlaced(t, leg.dst, start, oldDay)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("in the %s store:\n%+v\nwant\n%+v", leg.dst.Mode(), after, before)
		}
		if n := dailyCount(t, leg.src); n != 0 {
			t.Fatalf("cleanup left %d daily rows in the %s source", n, leg.src.Mode())
		}
	}
}

func reportTable(rep histstore.MigrateReport, table string) *histstore.TableReport {
	for i := range rep.Tables {
		if rep.Tables[i].Table == table {
			return &rep.Tables[i]
		}
	}
	return nil
}
