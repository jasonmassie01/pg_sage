// Package cost attributes an instance's dollar cost to the queries that
// used it and checks whether the instance is the right size (market-04).
// Every figure comes from operator-supplied rates (cost.rates) and carries
// their source and date; without rates there are no dollars. Right-sizing
// is advisory: it writes an info finding and never an action.
package cost

import (
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// SourceConfig names rates read from cost.rates.
const SourceConfig = "config"

// hoursPerMonth converts monthly prices (AWS and Google bill 730 h/month).
const hoursPerMonth = 730.0

// Rates prices one instance.
type Rates struct {
	Source          string
	Currency        string
	AsOf            time.Time
	ComputePerHour  float64
	StorageGBMonth  float64
	IOPSMonth       float64
	ProvisionedIOPS int
	// VCPUs is the operator-declared count (0: from telemetry).
	VCPUs int
	// ExtendedSupportPerVCPUHour is the operator's extended-support price
	// (drill-02a), set when HasExtendedSupport.
	ExtendedSupportPerVCPUHour float64
	HasExtendedSupport         bool
}

// RateSource finds the rates of a provider resource or, failing that, of
// a pg_sage database. ok is false when neither is priced.
type RateSource interface {
	Rates(resource, database string) (Rates, bool)
}

// ConfigRates are the cost.rates entries.
type ConfigRates struct {
	byResource map[string]Rates
	byDatabase map[string]Rates
}

// NewConfigRates indexes validated cost.rates entries (config.Load has
// refused invalid ones; an unparsable date is skipped, never priced).
func NewConfigRates(c config.CostConfig) *ConfigRates {
	out := &ConfigRates{byResource: map[string]Rates{}, byDatabase: map[string]Rates{}}
	for _, e := range c.Rates {
		at, err := e.AsOfDate()
		if err != nil || !e.HasCompute() {
			continue
		}
		r := Rates{Source: SourceConfig, Currency: e.Currency, AsOf: at,
			ComputePerHour: e.ComputePerHourValue(), StorageGBMonth: e.StorageGBMonth,
			IOPSMonth: e.IOPSMonth, ProvisionedIOPS: e.ProvisionedIOPS, VCPUs: e.VCPUs}
		if e.ExtendedSupportPerVCPUHour != nil {
			r.ExtendedSupportPerVCPUHour = *e.ExtendedSupportPerVCPUHour
			r.HasExtendedSupport = true
		}
		if e.Resource != "" {
			out.byResource[e.Resource] = r
		} else {
			out.byDatabase[e.Database] = r
		}
	}
	return out
}

// Rates implements RateSource: the resource's entry, else the database's.
func (c *ConfigRates) Rates(resource, database string) (Rates, bool) {
	if c == nil {
		return Rates{}, false
	}
	if r, ok := c.byResource[resource]; ok && resource != "" {
		return r, true
	}
	r, ok := c.byDatabase[database]
	return r, ok && database != ""
}

// empty reports a source with no entries.
func (c *ConfigRates) empty() bool {
	return c == nil || len(c.byResource)+len(c.byDatabase) == 0
}
