package trigger

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
)

// The rules each source applies, as pure functions of what it read.

// EnrollmentWindow is two complete weekly cycles of hourly rollups.
const EnrollmentWindow = 14 * 24 * time.Hour

// enrollmentCoveragePct is the share of the window's hours that must
// hold a rollup coverage row.
const enrollmentCoveragePct = 80

// Due picks the next review run's triggers from pending (oldest first).
// The oldest pending trigger opens a group that holds every trigger fired
// before it plus interval; the group is due once now reaches that close.
// A pending pre_upgrade bypasses the interval: every pending trigger runs
// at once.
func Due(pending []Trigger, interval time.Duration, now time.Time) ([]Trigger, bool) {
	if len(pending) == 0 {
		return nil, false
	}
	for _, t := range pending {
		if t.Kind == KindPreUpgrade {
			return pending, true
		}
	}
	closes := pending[0].FiredAt.Add(interval)
	group := []Trigger{pending[0]}
	for _, t := range pending[1:] {
		if t.FiredAt.Before(closes) {
			group = append(group, t)
		}
	}
	if now.Before(closes) {
		return nil, false
	}
	return group, true
}

// CalendarDue reports whether days have passed since the last full review
// finished. With no full review yet (last zero) it is enrollment's turn.
func CalendarDue(last, now time.Time, days int) bool {
	if last.IsZero() || days <= 0 {
		return false
	}
	return !now.Before(last.Add(time.Duration(days) * 24 * time.Hour))
}

// EnrollmentReady reports whether the rollups hold two complete weekly
// cycles: the oldest coverage hour is at least EnrollmentWindow old, and
// at least 80% of the window's hours are covered.
func EnrollmentReady(hours int, oldest, now time.Time) bool {
	if hours <= 0 || oldest.IsZero() || oldest.After(now.Add(-EnrollmentWindow)) {
		return false
	}
	return hours*100 >= enrollmentCoveragePct*int(EnrollmentWindow/time.Hour)
}

// DaySize is a database's size on one UTC day.
type DaySize struct {
	Day   time.Time
	Bytes int64
}

// Anomaly is a day whose size growth is a robust outlier.
type Anomaly struct {
	Day          time.Time
	GrowthBytes  int64
	MedianBytes  float64
	Z            float64
	HistoryDays  int
	ThresholdZ   float64
	FloorBytes   float64
	PreviousSize int64
}

const (
	growthZ          = 4
	minGrowthHistory = 7
	// growthFloorBytes keeps a small database's noise from firing: a day
	// must grow at least this much over the median growth.
	growthFloorBytes = 256 << 20
)

// GrowthAnomaly judges the newest day's growth (sizes is oldest first,
// one value a day) against the earlier days' growth with baseline.Judge:
// it fires at a robust z of 4 and at least 256 MiB over the median. Only
// growth between consecutive days counts; a gap breaks the series.
func GrowthAnomaly(sizes []DaySize) (Anomaly, bool) {
	var growth []float64
	var last Anomaly
	for i := 1; i < len(sizes); i++ {
		if !sizes[i].Day.Equal(sizes[i-1].Day.AddDate(0, 0, 1)) {
			growth = growth[:0]
			continue
		}
		growth = append(growth, float64(sizes[i].Bytes-sizes[i-1].Bytes))
		last = Anomaly{Day: sizes[i].Day, GrowthBytes: sizes[i].Bytes - sizes[i-1].Bytes,
			PreviousSize: sizes[i-1].Bytes}
	}
	n := len(growth)
	if n < minGrowthHistory+1 || !last.Day.Equal(sizes[len(sizes)-1].Day) {
		return Anomaly{}, false
	}
	s := baseline.Judge(growth[:n-1], growth[n-1], growthFloorBytes)
	last.MedianBytes, last.Z, last.HistoryDays = s.Median, s.Z, n-1
	last.ThresholdZ, last.FloorBytes = growthZ, growthFloorBytes
	return last, s.Exceeds(growthZ, growthFloorBytes)
}

// Facts are an instance's size facts; a zero field is unknown.
type Facts struct {
	Class       string `json:"class,omitempty"`
	VCPUs       int    `json:"vcpus,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
	Source      string `json:"source,omitempty"`
}

// InstanceChanged reports whether a fact known on both sides differs.
func InstanceChanged(prev, cur Facts) bool {
	return (prev.Class != "" && cur.Class != "" && prev.Class != cur.Class) ||
		(prev.VCPUs > 0 && cur.VCPUs > 0 && prev.VCPUs != cur.VCPUs) ||
		(prev.MemoryBytes > 0 && cur.MemoryBytes > 0 && prev.MemoryBytes != cur.MemoryBytes)
}

// TableSize is a table a migration touches, with its measured size.
type TableSize struct {
	Table string `json:"table"`
	Bytes int64  `json:"bytes"`
}

// LargeTables is the named tables strictly larger than threshold; an
// unset threshold (0) fires nothing.
func LargeTables(tables []TableSize, threshold int64) []TableSize {
	var out []TableSize
	if threshold <= 0 {
		return out
	}
	for _, t := range tables {
		if t.Table != "" && t.Bytes > threshold {
			out = append(out, t)
		}
	}
	return out
}

// migrationKey dedupes a large migration per table set and UTC day.
func migrationKey(tables []TableSize, now time.Time) string {
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Table)
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\x00")))
	return "migration:" + hex.EncodeToString(sum[:8]) + ":" +
		now.UTC().Format(time.DateOnly)
}
