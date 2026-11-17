package schemadebt

import (
	"slices"
	"strings"
	"testing"
)

// No concurrent access tests: Statement and Build are pure functions.

// A rate barely above zero at its low bound puts the high bound past
// what an int holds: the statement still says a positive number of days.
func TestStatement_HugeHighBoundIsNotANegativeNumber(t *testing.T) {
	r := Fit(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max), int4Max, false)
	r.DaysHigh = 4e19
	st := r.Statement()
	mustContain(t, "statement", st, "247 to 40000000000000000000 days")
	if strings.Contains(st, "-9223372036854775808") {
		t.Fatalf("statement %q overflowed the high bound", st)
	}
}

// The prediction source names the samples oldest to newest even when the
// series lists them out of order.
func TestBuild_SourceDatesAreOldestToNewestForUnorderedSamples(t *testing.T) {
	s := linear(ordersSeq, 1.9e9, 1e6, 5, int4Max)
	slices.Reverse(s.Points)
	c := only(t, build(t, schemaPack(t, nil, nil, ordersTable(1<<20)), keyInputs(s),
		Options{}), KindInt4Key)
	mustContain(t, "source", c.Prediction.Source, "5 samples from 2026-10-27 to 2026-10-31")
	mustContain(t, "reason", c.Reason, "exhausts in 247 days")
}
