package candidate

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

func TestWindowCoverage_ShorterThanConfiguredIsPartial(t *testing.T) {
	cases := []struct {
		packDays, windowDays int
		want                 string
	}{
		{90, 90, CoverageComplete},
		{89, 90, CoveragePartial},
		{100, 90, CoverageComplete},
		{14, 14, CoverageComplete},
		{14, 15, CoveragePartial},
	}
	for _, tc := range cases {
		p, err := reviewtest.Build(reviewtest.Spec{Database: "db1",
			Window: reviewtest.Window(tc.packDays)})
		if err != nil {
			t.Fatal(err)
		}
		cov := WindowCoverage(p, tc.windowDays)
		if cov.Status != tc.want || cov.WindowDays != tc.packDays {
			t.Errorf("pack %d days, window %d: coverage %+v, want %s over %d days",
				tc.packDays, tc.windowDays, cov, tc.want, tc.packDays)
		}
		if tc.want == CoveragePartial && len(cov.Notes) == 0 {
			t.Errorf("partial coverage says nothing about why")
		}
	}
}

func TestWindowCoverage_NilPackIsPartialAndSaysSo(t *testing.T) {
	cov := WindowCoverage(nil, 90)
	if cov.Status != CoveragePartial || len(cov.Missing) == 0 || cov.Missing[0] != "pack" {
		t.Fatalf("nil pack coverage %+v, want partial with pack missing", cov)
	}
}

func TestCoverage_MissingMakesItPartial(t *testing.T) {
	cov := Coverage{Status: CoverageComplete}
	cov.AddMissing("indexes")
	cov.AddMissing("indexes")
	if cov.Status != CoveragePartial || len(cov.Missing) != 1 || cov.Missing[0] != "indexes" {
		t.Fatalf("coverage %+v, want partial with indexes missing once", cov)
	}
}
