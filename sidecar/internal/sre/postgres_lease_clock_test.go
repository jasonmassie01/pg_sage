package sre

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// sre-12: a lease's deadlines are host-monotonic times derived from the
// database's remaining time, anchored on the moment the query was sent.
// No concurrent access tests here: hostDeadline and setDeadlines are pure
// and touch only the lease value they are given.

func TestHostDeadline_AddsTheRemainingToTheSendTime(t *testing.T) {
	sent := time.Now()
	for _, tc := range []struct {
		left float64
		want time.Duration
	}{
		{1.5, 1500 * time.Millisecond},
		{30, 30 * time.Second},
		{0.000001, time.Microsecond},
		{3600, time.Hour},
	} {
		got := hostDeadline(sent, tc.left)
		if d := got.Sub(sent); d != tc.want {
			t.Errorf("hostDeadline(sent, %v) = sent%+v, want sent%+v", tc.left, d, tc.want)
		}
	}
}

// The deadline keeps the send time's monotonic reading, so time.Until on
// it is immune to wall-clock steps on the host.
func TestHostDeadline_KeepsTheMonotonicReading(t *testing.T) {
	got := hostDeadline(time.Now(), 10)
	if !strings.Contains(got.String(), " m=") {
		t.Fatalf("deadline %v lost the monotonic reading of its send time", got)
	}
	wall := time.Now().Round(0) // a wall-only time, as a database timestamp is
	if s := hostDeadline(wall, 10).String(); strings.Contains(s, " m=") {
		t.Fatalf("a wall-only send time gained a monotonic reading: %s", s)
	}
}

func TestHostDeadline_NonFiniteOrHugeRemainingYieldsTheSendTime(t *testing.T) {
	sent := time.Now()
	for _, left := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300, -1e300,
		float64(math.MaxInt64)} {
		got := hostDeadline(sent, left)
		if !got.Equal(sent) {
			t.Errorf("hostDeadline(sent, %v) = sent%+v, want sent (never a far-future "+
				"deadline)", left, got.Sub(sent))
		}
	}
}

func TestHostDeadline_NegativeRemainingIsInThePast(t *testing.T) {
	sent := time.Now()
	got := hostDeadline(sent, -2)
	if d := got.Sub(sent); d != -2*time.Second {
		t.Fatalf("hostDeadline(sent, -2) = sent%+v, want sent-2s", d)
	}
	if time.Until(got) >= 0 {
		t.Fatalf("a lapsed remaining must give a deadline in the past, time.Until = %v",
			time.Until(got))
	}
}

func TestHostDeadline_Boundaries(t *testing.T) {
	sent := time.Now()
	if left := time.Until(hostDeadline(sent, 0)); left > 0 {
		t.Fatalf("remaining 0: time.Until = %v, want <= 0", left)
	}
	ttl := DefaultLimits().LeaseTTL
	if left := time.Until(hostDeadline(sent, ttl.Seconds())); left > ttl || left <= 0 {
		t.Fatalf("remaining = LeaseTTL: time.Until = %v, want in (0, %v]", left, ttl)
	}
}

func TestLeaseSetDeadlines_SetsBothFromOneSendTime(t *testing.T) {
	sent := time.Now()
	var l Lease
	if err := l.setDeadlines(sent, 30, 90.25); err != nil {
		t.Fatalf("setDeadlines: %v", err)
	}
	if l.Until.Sub(sent) != 30*time.Second {
		t.Fatalf("Until = sent%+v, want sent+30s", l.Until.Sub(sent))
	}
	if l.SegmentDeadline.Sub(sent) != 90250*time.Millisecond {
		t.Fatalf("SegmentDeadline = sent%+v, want sent+1m30.25s", l.SegmentDeadline.Sub(sent))
	}
}

func TestLeaseSetDeadlines_RefusesAZeroSendTime(t *testing.T) {
	var l Lease
	err := l.setDeadlines(time.Time{}, 30, 90)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("setDeadlines(zero sent) = %v, want ErrInvalidRequest", err)
	}
	if !l.Until.IsZero() || !l.SegmentDeadline.IsZero() {
		t.Fatalf("refused deadlines were set: until=%v segment=%v", l.Until,
			l.SegmentDeadline)
	}
}

func TestLeaseSetDeadlines_NonFiniteRemainingIsDueNow(t *testing.T) {
	sent := time.Now()
	var l Lease
	if err := l.setDeadlines(sent, math.NaN(), math.Inf(1)); err != nil {
		t.Fatalf("setDeadlines: %v", err)
	}
	if !l.Until.Equal(sent) || !l.SegmentDeadline.Equal(sent) {
		t.Fatalf("non-finite remaining gave until=sent%+v segment=sent%+v, want sent",
			l.Until.Sub(sent), l.SegmentDeadline.Sub(sent))
	}
}
