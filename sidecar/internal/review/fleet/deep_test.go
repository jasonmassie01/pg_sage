package fleet

import (
	"fmt"
	"testing"
	"time"
)

// Tier 4: deep reviews for the top outliers, at most fleet_deep_max per
// calendar month, with tokens split by need.
// No concurrent access tests: selection and allocation are pure functions.

func outliers(n int) []Outlier {
	out := make([]Outlier, n)
	for i := range out {
		out[i] = Outlier{Database: fmt.Sprintf("db%04d", i+1), Score: float64(100 - i)}
	}
	return out
}

func TestSelectDeep_ThirtyOfThirtyOneAndTheLastWaits(t *testing.T) {
	pick, rest := SelectDeep(outliers(31), 30)
	if len(pick) != 30 || len(rest) != 1 || rest[0].Database != "db0031" ||
		pick[0].Database != "db0001" {
		t.Fatalf("pick %d rest %+v, want 30 and db0031 waiting", len(pick), rest)
	}
	pick, rest = SelectDeep(outliers(30), 30)
	if len(pick) != 30 || len(rest) != 0 {
		t.Fatalf("30 of 30: pick %d rest %d", len(pick), len(rest))
	}
}

func TestSelectDeep_CarriedGoFirstAndZeroRoomWaitsAll(t *testing.T) {
	in := outliers(3)
	in[2].Carried = true
	pick, rest := SelectDeep(in, 1)
	if len(pick) != 1 || pick[0].Database != "db0003" || len(rest) != 2 {
		t.Fatalf("pick %+v, want the carried db0003 first", pick)
	}
	for _, room := range []int{0, -4} {
		pick, rest = SelectDeep(outliers(3), room)
		if len(pick) != 0 || len(rest) != 3 {
			t.Fatalf("room %d: pick %d rest %d", room, len(pick), len(rest))
		}
	}
	if pick, rest := SelectDeep(nil, 30); len(pick) != 0 || len(rest) != 0 {
		t.Fatal("nothing to select gave a selection")
	}
}

func TestAllocateTokens_ByNeed(t *testing.T) {
	pick := []Outlier{{Database: "a", Score: 40}, {Database: "b", Score: 1},
		{Database: "c", Score: 1}}
	got := AllocateTokens(pick)
	total := got["a"] + got["b"] + got["c"]
	if total > 3*DeepTokensPerReview || total < 3*DeepTokensPerReview-3 {
		t.Fatalf("allocated %d, want about %d", total, 3*DeepTokensPerReview)
	}
	if got["a"] <= got["b"] || got["b"] < DeepTokensPerReview/2 || got["b"] != got["c"] {
		t.Fatalf("allocation %v: the neediest must get most, none under half", got)
	}
	if len(AllocateTokens(nil)) != 0 {
		t.Fatal("no picks were allocated tokens")
	}
}

func TestPeriod_IsTheUTCCalendarMonth(t *testing.T) {
	at := time.Date(2026, 11, 30, 23, 59, 0, 0, time.FixedZone("x", -6*3600))
	if got := Period(at); got != "2026-12" {
		t.Fatalf("Period = %s, want 2026-12 (UTC)", got)
	}
	for in, want := range map[string]string{"2026-11": "2026-12", "2026-12": "2027-01",
		"bad": "", "": ""} {
		if got := NextPeriod(in); got != want {
			t.Errorf("NextPeriod(%q) = %q, want %q", in, got, want)
		}
	}
}
