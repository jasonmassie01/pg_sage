package forecaster

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/runway"
)

// Disk capacity sources of forecast_disk_growth and /forecasts/growth
// (forecast-06): the provider's fresh measurement, the operator's
// forecaster.disk_capacity_bytes, neither, or storage the provider grows
// without a bound. The values are the runway monitor's, so a finding and a
// disk runway name a source the same way.
const (
	CapacityProviderTelemetry = runway.CapacityProviderTelemetry
	CapacityDeclared          = runway.CapacityDeclared
	CapacityUnbounded         = runway.CapacityUnbounded
	CapacityUndeclared        = "undeclared"
)

// diskCapacityKey is the setting that declares a self-hosted capacity.
const diskCapacityKey = "forecaster.disk_capacity_bytes"

// managedStorageAuthority is the finding that decides on managed storage
// inside a week: the provider's own free-storage runway.
const managedStorageAuthority = "managed_storage_runway"

// Capacity is the disk capacity a growth trend is judged against. Bytes is
// 0 unless Source is provider_telemetry or declared. Managed is a database
// with a provider capacity source, whether or not it answered.
type Capacity struct {
	Bytes   float64
	Source  string
	Managed bool
}

// ResolveCapacity prefers a fresh bounded provider capacity, then the
// declared one. A provider that reports unbounded storage stays unbounded:
// the declared value is not used in its place, since the provider says
// the storage grows. A nil source is no provider: the declared value only.
func ResolveCapacity(ctx context.Context, declared int64, src runway.CapacitySource) Capacity {
	c := Capacity{Managed: src != nil}
	if src != nil {
		bytes, source, ok := src(ctx)
		if ok && bytes > 0 && !math.IsInf(bytes, 1) {
			c.Bytes, c.Source = bytes, CapacityProviderTelemetry
			return c
		}
		if source == CapacityUnbounded {
			c.Source = CapacityUnbounded
			return c
		}
	}
	if declared > 0 {
		c.Bytes, c.Source = float64(declared), CapacityDeclared
		return c
	}
	c.Source = CapacityUndeclared
	return c
}

// known reports a bounded capacity to project against.
func (c Capacity) known() bool {
	return c.Bytes > 0 && (c.Source == CapacityProviderTelemetry || c.Source == CapacityDeclared)
}

// DaysUntilFull is (capacity - current) / slope in days, never negative;
// false without a known capacity or a growing size.
func (c Capacity) DaysUntilFull(currentBytes, bytesPerDay float64) (float64, bool) {
	if !c.known() || !(bytesPerDay > 0) || math.IsNaN(currentBytes) {
		return 0, false
	}
	return math.Max(0, (c.Bytes-currentBytes)/bytesPerDay), true
}

// AddTo writes the capacity fields into detail: capacity_source always,
// capacity_bytes and days_until_full when known, the config key when
// undeclared, and the managed-storage authority on a managed database.
func (c Capacity) AddTo(detail map[string]any, currentBytes, bytesPerDay float64) {
	source := c.Source
	if source == "" {
		source = CapacityUndeclared
	}
	detail["capacity_source"] = source
	if c.known() {
		detail["capacity_bytes"] = c.Bytes
	}
	if days, ok := c.DaysUntilFull(currentBytes, bytesPerDay); ok {
		detail["days_until_full"] = days
	}
	if source == CapacityUndeclared {
		detail["config_key"] = diskCapacityKey
	}
	if c.Managed {
		detail["authority_under_7_days"] = managedStorageAuthority
	}
}

// withDiskCapacity adds the capacity to forecast_disk_growth findings,
// and the time to full (or how to get one) to their recommendation.
func withDiskCapacity(findings []analyzer.Finding, c Capacity) []analyzer.Finding {
	for i := range findings {
		f := &findings[i]
		current, _ := f.Detail["current_size_bytes"].(float64)
		rate, _ := f.Detail["growth_rate_bytes_day"].(float64)
		c.AddTo(f.Detail, current, rate)
		f.Recommendation += capacityAdvice(f.Detail)
		f.Title += projectedFull(f.Detail)
	}
	return findings
}

// projectedFull is the title's projected full date (forecast-08): the
// newest measured day's date plus days_until_full, when both are known
// and the date is within maxProjectedDays.
func projectedFull(detail map[string]any) string {
	days, ok := detail["days_until_full"].(float64)
	asOf, dated := detail["as_of_day"].(time.Time)
	if !ok || !dated {
		return ""
	}
	date, ok := projectedDate(asOf, days)
	if !ok {
		return ""
	}
	detail["projected_full_date"] = date
	return fmt.Sprintf(", full by %s", date)
}

// capacityAdvice is the recommendation's sentence on the capacity.
func capacityAdvice(detail map[string]any) string {
	advice := ""
	if days, ok := detail["days_until_full"].(float64); ok {
		advice = fmt.Sprintf(" At this rate the database fills the %s capacity in %.0f days.",
			detail["capacity_source"], days)
	} else if detail["capacity_source"] == CapacityUndeclared {
		advice = " No disk capacity is known, so there is no time to full: set " +
			diskCapacityKey + "."
	}
	if detail["authority_under_7_days"] == managedStorageAuthority {
		advice += " On managed storage, " + managedStorageAuthority +
			" (the provider's free-storage trend) decides inside 7 days."
	}
	return advice
}
