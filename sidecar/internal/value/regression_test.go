package value

import (
	"reflect"
	"testing"
)

// G2-B25: report rows must have a deterministic order (days ascending,
// databases by minutes descending then name) instead of map order.
func TestRegression_ValueRowsAreSorted(t *testing.T) {
	days := map[string]float64{
		"2026-09-03": 1, "2026-09-01": 2, "2026-09-02": 3, "2026-09-04": 4,
	}
	for i := 0; i < 20; i++ {
		got := dayRows(days)
		want := []DayMinutes{
			{"2026-09-01", 2}, {"2026-09-02", 3},
			{"2026-09-03", 1}, {"2026-09-04", 4},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("dayRows = %+v, want %+v", got, want)
		}
	}
	dbs := map[string]float64{"b": 5, "a": 5, "c": 9, "d": 1}
	for i := 0; i < 20; i++ {
		got := databaseRows(dbs)
		want := []DatabaseMinutes{{"c", 9}, {"a", 5}, {"b", 5}, {"d", 1}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("databaseRows = %+v, want %+v", got, want)
		}
	}
}
