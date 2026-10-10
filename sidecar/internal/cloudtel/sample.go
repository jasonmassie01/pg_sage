// Package cloudtel reads host telemetry of managed PostgreSQL services
// that PostgreSQL itself cannot see: CPU, memory, storage, IOPS and
// throughput, replica lag and DB load, from Amazon CloudWatch and
// Performance Insights (RDS, Aurora) and Google Cloud Monitoring (Cloud
// SQL). Credentials come only from the providers' standard chains; without
// them the telemetry is unavailable with a reason and nothing else changes.
// Telemetry is evidence for guards; it never grants or raises trust.
package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

const (
	// MaxPointAge is the oldest datapoint a guard uses (CloudWatch and
	// Cloud Monitoring publish one-minute points a few minutes late).
	MaxPointAge = 10 * time.Minute
	// MaxClockSkew is the largest provider/local clock disagreement
	// tolerated before freshness can no longer be judged.
	MaxClockSkew = 5 * time.Minute

	MemorySourcePerformanceInsights = "performance_insights"
	MemorySourceInstanceClass       = "instance_class"
	MemorySourceCloudMonitoring     = "cloud_monitoring"
	MemorySourceMachineTier         = "machine_tier"
)

var (
	// ErrUnavailable: telemetry cannot be collected here (no credentials,
	// no identity). Callers keep today's behavior.
	ErrUnavailable   = errors.New("cloud telemetry unavailable")
	ErrNoCredentials = fmt.Errorf("%w: no credentials", ErrUnavailable)
	ErrIdentity      = fmt.Errorf("%w: database identity unknown", ErrUnavailable)
	ErrAuth          = errors.New("cloud telemetry access denied")
	ErrThrottled     = errors.New("cloud telemetry throttled")
	ErrClockSkew     = errors.New("cloud telemetry clock skew")
	ErrMalformed     = errors.New("cloud telemetry response malformed")
	ErrProvider      = errors.New("cloud telemetry provider error")
)

// Point is one measurement and when the provider measured it.
type Point struct {
	Value float64   `json:"value"`
	At    time.Time `json:"at"`
}

// fresh reports a finite, non-negative value measured within MaxPointAge
// and not in the future beyond MaxClockSkew.
func (p *Point) fresh(now time.Time) bool {
	if p == nil || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 ||
		p.At.IsZero() {
		return false
	}
	return !p.At.After(now.Add(MaxClockSkew)) && now.Sub(p.At) <= MaxPointAge
}

// Sample is one collection. Unknown values are nil, never zero.
type Sample struct {
	Provider              string             `json:"provider"`
	Resource              string             `json:"resource"`
	CollectedAt           time.Time          `json:"collected_at"`
	CPUPct                *Point             `json:"cpu_pct,omitempty"`
	MemoryTotalBytes      *Point             `json:"memory_total_bytes,omitempty"`
	FreeableMemoryBytes   *Point             `json:"freeable_memory_bytes,omitempty"`
	FreeStorageBytes      *Point             `json:"free_storage_bytes,omitempty"`
	AllocatedStorageBytes *Point             `json:"allocated_storage_bytes,omitempty"`
	StorageCapacityBytes  *Point             `json:"storage_capacity_bytes,omitempty"`
	ReadIOPS              *Point             `json:"read_iops,omitempty"`
	WriteIOPS             *Point             `json:"write_iops,omitempty"`
	ReadBytesPerSec       *Point             `json:"read_bytes_per_sec,omitempty"`
	WriteBytesPerSec      *Point             `json:"write_bytes_per_sec,omitempty"`
	ReplicaLagSeconds     *Point             `json:"replica_lag_seconds,omitempty"`
	Connections           *Point             `json:"connections,omitempty"`
	DBLoad                *Point             `json:"db_load,omitempty"`
	DBLoadByWait          map[string]float64 `json:"db_load_by_wait,omitempty"`
	MemoryTotalSource     string             `json:"memory_total_source,omitempty"`
	// StorageAutoGrows: the provider grows storage without a bound pg_sage
	// knows (Aurora's cluster volume, unlimited Cloud SQL auto-resize).
	StorageAutoGrows bool     `json:"storage_auto_grows"`
	Missing          []string `json:"missing,omitempty"`
	// Backup is the instance's backup posture (AP-11); nil when not read.
	Backup *BackupPosture `json:"backup,omitempty"`
}

// Source collects one sample from a provider.
type Source interface {
	Provider() string
	Collect(ctx context.Context, now time.Time) (Sample, error)
}

// CPU is fresh host CPU utilization; an error wraps
// verify.ErrLoadTelemetryUnavailable (unknown, never 0%).
func (s Sample) CPU(now time.Time) (float64, error) {
	if !s.CPUPct.fresh(now) || s.CPUPct.Value > 100 {
		return 0, fmt.Errorf("%w: provider CPU missing, stale or invalid",
			verify.ErrLoadTelemetryUnavailable)
	}
	return s.CPUPct.Value, nil
}

// HostMemory is fresh total and available memory in bytes (0 = unknown).
// An available figure above the total is dropped as inconsistent.
func (s Sample) HostMemory(now time.Time) (total, available int64) {
	if !s.MemoryTotalBytes.fresh(now) || s.MemoryTotalBytes.Value == 0 {
		return 0, 0
	}
	total = int64(s.MemoryTotalBytes.Value)
	if s.FreeableMemoryBytes.fresh(now) && s.FreeableMemoryBytes.Value <= float64(total) {
		available = int64(s.FreeableMemoryBytes.Value)
	}
	return total, available
}

// EffectiveFreeStorage is the free space up to the hard capacity (free in
// the current allocation plus autoscaling headroom); ok is false when
// storage is unbounded or not measured.
func (s Sample) EffectiveFreeStorage(now time.Time) (free, capacity float64, ok bool) {
	if s.StorageAutoGrows || !s.FreeStorageBytes.fresh(now) ||
		!s.StorageCapacityBytes.fresh(now) || s.StorageCapacityBytes.Value == 0 {
		return 0, 0, false
	}
	capacity = s.StorageCapacityBytes.Value
	allocated := capacity
	if s.AllocatedStorageBytes.fresh(now) && s.AllocatedStorageBytes.Value <= capacity {
		allocated = s.AllocatedStorageBytes.Value
	}
	free = min(s.FreeStorageBytes.Value+(capacity-allocated), capacity)
	return free, capacity, true
}

// clone deep-copies the sample for callers.
func (s Sample) clone() Sample {
	c := s
	for _, p := range []**Point{&c.CPUPct, &c.MemoryTotalBytes, &c.FreeableMemoryBytes,
		&c.FreeStorageBytes, &c.AllocatedStorageBytes, &c.StorageCapacityBytes, &c.ReadIOPS,
		&c.WriteIOPS, &c.ReadBytesPerSec, &c.WriteBytesPerSec, &c.ReplicaLagSeconds,
		&c.Connections, &c.DBLoad} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	if s.DBLoadByWait != nil {
		c.DBLoadByWait = make(map[string]float64, len(s.DBLoadByWait))
		for k, v := range s.DBLoadByWait {
			c.DBLoadByWait[k] = v
		}
	}
	c.Missing = append([]string(nil), s.Missing...)
	return c
}
