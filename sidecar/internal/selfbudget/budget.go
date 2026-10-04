// Package selfbudget is pg_sage's declared budget for itself (roadmap
// phase 3, "cheap, self-aware observer"): the sidecar's CPU per collector
// cycle, its database time and shared blocks per hour, and the size of
// the sage schema. It compares what pg_sage measured about itself
// (selfcost, the process CPU, the busy time of its loops) with what it
// promised, and says which resources are over budget.
package selfbudget

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// Resource names one budgeted resource.
type Resource string

// The budgeted resources, in report order.
const (
	ResourceCPU     Resource = "cpu"
	ResourceDBTime  Resource = "db_time"
	ResourceIO      Resource = "io"
	ResourceStorage Resource = "storage"
)

// Units of each resource's figures.
const (
	unitCPU     = "ms CPU per collector cycle"
	unitDBTime  = "ms database time per hour"
	unitIO      = "blocks per hour"
	unitStorage = "bytes"
)

// Budget is what pg_sage may use; a zero field disables that resource.
type Budget struct {
	CPUMsPerCycle   float64 // sidecar process CPU per collector cycle
	DBTimeMsPerHour float64 // execution + planning time of pg_sage's statements
	BlocksPerHour   float64 // shared blocks hit + read by pg_sage's statements
	StorageBytes    int64   // the sage schema: tables, TOAST and indexes
}

// Usage is what pg_sage measured about itself; a figure counts only when
// its Known flag is set.
type Usage struct {
	CPUKnown        bool
	CPUMsPerCycle   float64
	DBKnown         bool // DBTimeMsPerHour and BlocksPerHour
	DBTimeMsPerHour float64
	BlocksPerHour   float64
	StorageKnown    bool
	StorageBytes    int64
}

// Breach is one resource over budget.
type Breach struct {
	Resource Resource
	Used     float64
	Limit    float64
	Unit     string
}

// Share is how many times the budget the resource used (0 without a
// limit).
func (b Breach) Share() float64 {
	if b.Limit <= 0 {
		return 0
	}
	return b.Used / b.Limit
}

func (b Breach) String() string {
	return fmt.Sprintf("%s %.0f %s (budget %.0f)", b.Resource, b.Used, b.Unit, b.Limit)
}

// Validate refuses a negative or non-finite budget.
func (b Budget) Validate() error {
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"cpu_ms_per_cycle", b.CPUMsPerCycle},
		{"db_time_ms_per_hour", b.DBTimeMsPerHour},
		{"blocks_per_hour", b.BlocksPerHour},
		{"storage", float64(b.StorageBytes)},
	} {
		if f.v < 0 || math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return fmt.Errorf("self budget %s must be a finite number >= 0, got %v", f.name,
				f.v)
		}
	}
	return nil
}

// Check lists the resources over budget: known usage strictly above an
// enabled (positive) budget. Unknown usage proves nothing and never
// breaches.
func Check(b Budget, u Usage) []Breach {
	var out []Breach
	add := func(known bool, r Resource, used, limit float64, unit string) {
		if known && limit > 0 && used > limit {
			out = append(out, Breach{Resource: r, Used: used, Limit: limit, Unit: unit})
		}
	}
	add(u.CPUKnown, ResourceCPU, u.CPUMsPerCycle, b.CPUMsPerCycle, unitCPU)
	add(u.DBKnown, ResourceDBTime, u.DBTimeMsPerHour, b.DBTimeMsPerHour, unitDBTime)
	add(u.DBKnown, ResourceIO, u.BlocksPerHour, b.BlocksPerHour, unitIO)
	add(u.StorageKnown, ResourceStorage, float64(u.StorageBytes), float64(b.StorageBytes),
		unitStorage)
	return out
}

// FromCost is the database side of the usage: DB time and blocks per
// hour from the per-cycle cost, the sage schema's size. CPU is the
// process's and comes from a CPUMeter.
func FromCost(c selfcost.Cost) Usage {
	u := Usage{StorageKnown: c.SchemaBytes > 0, StorageBytes: c.SchemaBytes}
	if c.Known && c.DBTimeKnown && c.CycleSeconds > 0 {
		perHour := 3600 / c.CycleSeconds
		u.DBKnown = true
		u.DBTimeMsPerHour = c.DBTimeMsPerCycle * perHour
		u.BlocksPerHour = c.BlocksPerCycle * perHour
	}
	return u
}
