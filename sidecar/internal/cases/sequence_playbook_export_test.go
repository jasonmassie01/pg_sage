package cases

import (
	"strings"
	"testing"
	"time"
)

// SequenceCapacityScript is the playbook's script for a sequence named by
// catalog names (review-02d): quoted, with the measured rate or the
// not-measured line.
func TestSequenceCapacityScript_QuotesTheNameAndStatesTheRate(t *testing.T) {
	read := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	got := SequenceCapacityScript("public.orders_id_seq", map[string]any{
		DetailRunwayRatePerS: 1e6 / 86400.0, DetailRunwaySecondsToLimit: 247.0 * 86400,
		DetailRunwayReadAt: read, "increment": 1.0})
	for _, want := range []string{`'"public"."orders_id_seq"'`,
		"pg_sage never runs this script", "Measured rate: rises",
		"limit is reached in about 247 days (2027-07-05)", "ALTER SEQUENCE"} {
		if !strings.Contains(got, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	none := SequenceCapacityScript("public.orders_id_seq", nil)
	if !strings.Contains(none, "Rate: not measured yet") ||
		strings.Contains(none, "Measured rate") {
		t.Fatalf("script without a forecast states a rate:\n%s", none)
	}
}
