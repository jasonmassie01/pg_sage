package cloudtel

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Limits are the host guards' floors and ceilings; a zero value disables
// that guard (it never makes a guard stricter).
type Limits struct {
	MaxReplicaLagSeconds  float64 `json:"max_replica_lag_seconds"`
	MinFreeStoragePct     float64 `json:"min_free_storage_pct"`
	MinStorageRunwayHours float64 `json:"min_storage_runway_hours"`
	MinAvailableMemoryPct float64 `json:"min_available_memory_pct"`
}

// DefaultLimits: 30 s of replica lag, 10% free storage, 24 h of storage
// runway, 5% available memory.
func DefaultLimits() Limits {
	return Limits{MaxReplicaLagSeconds: 30, MinFreeStoragePct: 10,
		MinStorageRunwayHours: 24, MinAvailableMemoryPct: 5}
}

// Validate refuses negative, non-finite and over-100% limits.
func (l Limits) Validate() error {
	for name, v := range map[string]float64{
		"max_replica_lag_seconds":  l.MaxReplicaLagSeconds,
		"min_free_storage_pct":     l.MinFreeStoragePct,
		"min_storage_runway_hours": l.MinStorageRunwayHours,
		"min_available_memory_pct": l.MinAvailableMemoryPct,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("cloud telemetry limit %s must be a finite value >= 0", name)
		}
	}
	if l.MinFreeStoragePct > 100 || l.MinAvailableMemoryPct > 100 {
		return errors.New("cloud telemetry percentage limits must be at most 100")
	}
	return nil
}

// Runway is how long the effective free storage lasts at its recent rate.
type Runway struct {
	Known             bool          `json:"known"`
	Hours             float64       `json:"hours"`
	SlopeBytesPerHour float64       `json:"slope_bytes_per_hour"`
	Points            int           `json:"points"`
	Span              time.Duration `json:"span"`
}

const (
	runwayWindow    = 24 * time.Hour
	runwayMinPoints = 6
	runwayMinSpan   = 30 * time.Minute
)

// StorageRunway fits free bytes over the last 24 hours by least squares.
// It needs at least 6 points spanning 30 minutes; a flat or growing series
// has an infinite runway.
func StorageRunway(history []Point, now time.Time) Runway {
	var pts []Point
	for _, p := range history {
		if !p.At.After(now.Add(MaxClockSkew)) && now.Sub(p.At) <= runwayWindow &&
			!math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
			pts = append(pts, p)
		}
	}
	r := Runway{Points: len(pts)}
	if len(pts) == 0 {
		return r
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	r.Span = pts[len(pts)-1].At.Sub(pts[0].At)
	if len(pts) < runwayMinPoints || r.Span < runwayMinSpan {
		return r
	}
	r.SlopeBytesPerHour = slopePerHour(pts)
	r.Known = true
	latest := pts[len(pts)-1].Value
	if r.SlopeBytesPerHour >= 0 {
		r.Hours = math.Inf(1)
		return r
	}
	r.Hours = latest / -r.SlopeBytesPerHour
	return r
}

func slopePerHour(pts []Point) float64 {
	origin := pts[0].At
	var sx, sy, sxx, sxy float64
	n := float64(len(pts))
	for _, p := range pts {
		x := p.At.Sub(origin).Hours()
		sx += x
		sy += p.Value
		sxx += x * x
		sxy += x * p.Value
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// Withhold returns why autonomous heavy maintenance (index builds) should
// wait: replica lag over the ceiling, effective free storage or its runway
// under the floor, available memory under the floor. Unknown or stale
// measurements add nothing: without telemetry admission is unchanged.
func Withhold(s Sample, runway Runway, now time.Time, l Limits) []string {
	var out []string
	if l.MaxReplicaLagSeconds > 0 && s.ReplicaLagSeconds.fresh(now) &&
		s.ReplicaLagSeconds.Value > l.MaxReplicaLagSeconds {
		out = append(out, fmt.Sprintf("replica lag %.0fs exceeds %.0fs",
			s.ReplicaLagSeconds.Value, l.MaxReplicaLagSeconds))
	}
	if free, capacity, ok := s.EffectiveFreeStorage(now); ok {
		pct := 100 * free / capacity
		if l.MinFreeStoragePct > 0 && pct < l.MinFreeStoragePct {
			out = append(out, fmt.Sprintf("free storage %.1f%% is below %.0f%%", pct,
				l.MinFreeStoragePct))
		}
		if l.MinStorageRunwayHours > 0 && runway.Known && runway.Hours < l.MinStorageRunwayHours {
			out = append(out, fmt.Sprintf("storage runs out in %.1fh (below %.0fh)",
				runway.Hours, l.MinStorageRunwayHours))
		}
	}
	if total, _ := s.HostMemory(now); total > 0 && s.FreeableMemoryBytes.fresh(now) &&
		s.FreeableMemoryBytes.Value <= float64(total) && l.MinAvailableMemoryPct > 0 {
		pct := 100 * s.FreeableMemoryBytes.Value / float64(total)
		if pct < l.MinAvailableMemoryPct {
			out = append(out, fmt.Sprintf("available memory %.1f%% is below %.0f%%", pct,
				l.MinAvailableMemoryPct))
		}
	}
	return out
}
