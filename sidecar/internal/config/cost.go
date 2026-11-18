package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// CostConfig is operator-supplied pricing for dollar cost attribution and
// instance right-sizing (market-04). pg_sage ships no price tables: every
// figure comes from these rates, carries their date, and nothing is
// priced without them.
type CostConfig struct {
	Rates []CostRate `yaml:"rates" doc:"Operator-supplied prices, one entry per provider resource (as cloud telemetry names it, e.g. rds:us-east-1/orders) or per pg_sage database name. Empty: no dollar figures anywhere."`
}

// CostRate prices one instance. Exactly one compute price applies:
// compute_per_hour, or dbu_per_hour or cu_per_hour times compute_units.
type CostRate struct {
	Resource        string   `yaml:"resource" doc:"Provider resource the rate prices, as cloud telemetry status names it (rds:<region>/<instance>, aurora:<region>/<instance>, cloud-sql:<project>:<instance>). Set this or database."`
	Database        string   `yaml:"database" doc:"pg_sage database name the rate prices (self-managed or when the resource is not known). Set this or resource."`
	Currency        string   `yaml:"currency" doc:"ISO 4217 currency of every price in the entry, e.g. USD or EUR."`
	AsOf            string   `yaml:"as_of" doc:"Date (YYYY-MM-DD) the prices were read from the provider's price list; shown next to every figure."`
	ComputePerHour  *float64 `yaml:"compute_per_hour" doc:"Instance compute price per hour (on-demand, reserved or negotiated, as you pay it)."`
	DBUPerHour      *float64 `yaml:"dbu_per_hour" doc:"Price per DBU-hour; with compute_units, instead of compute_per_hour."`
	CUPerHour       *float64 `yaml:"cu_per_hour" doc:"Price per capacity-unit hour (Lakebase); with compute_units, instead of compute_per_hour."`
	ComputeUnits    float64  `yaml:"compute_units" doc:"Fixed DBUs or capacity units the instance runs at; required with dbu_per_hour or cu_per_hour."`
	StorageGBMonth  float64  `yaml:"storage_gb_month" doc:"Storage price per GB-month, applied to the allocated storage cloud telemetry reports."`
	IOPSMonth       float64  `yaml:"iops_month" doc:"Price per provisioned IOPS-month; requires provisioned_iops."`
	ProvisionedIOPS int      `yaml:"provisioned_iops" doc:"Provisioned IOPS the iops_month price applies to."`
	VCPUs           int      `yaml:"vcpus" doc:"vCPUs of the instance when telemetry cannot tell (self-managed); used for the idle share. 0: from telemetry."`
	// ExtendedSupportPerVCPUHour prices the upgrade brief's cost of waiting
	// (drill-02a); it is not a compute price.
	ExtendedSupportPerVCPUHour *float64 `yaml:"extended_support_per_vcpu_hour" doc:"Optional provider extended-support price per vCPU-hour (RDS, Aurora, Azure) once a major leaves standard support; with vcpus it prices the upgrade brief's cost of waiting. Unset: the brief says unpriced."`
}

// costCurrencies are the ISO 4217 codes a rate may name.
var costCurrencies = map[string]bool{"USD": true, "EUR": true, "GBP": true, "JPY": true,
	"CAD": true, "AUD": true, "CHF": true, "SEK": true, "NOK": true, "DKK": true,
	"NZD": true, "SGD": true, "HKD": true, "INR": true, "BRL": true, "MXN": true,
	"ZAR": true, "KRW": true, "CNY": true, "PLN": true, "ILS": true}

// maxCostVCPUs bounds a declared vCPU count.
const maxCostVCPUs = 1024

// Configured reports whether any rate is set.
func (c CostConfig) Configured() bool { return len(c.Rates) > 0 }

// HasCompute reports whether the entry sets a compute price.
func (r CostRate) HasCompute() bool {
	return r.ComputePerHour != nil || r.DBUPerHour != nil || r.CUPerHour != nil
}

// ComputePerHourValue is the instance's compute price per hour.
func (r CostRate) ComputePerHourValue() float64 {
	switch {
	case r.ComputePerHour != nil:
		return *r.ComputePerHour
	case r.DBUPerHour != nil:
		return *r.DBUPerHour * r.ComputeUnits
	case r.CUPerHour != nil:
		return *r.CUPerHour * r.ComputeUnits
	}
	return 0
}

// AsOfDate parses as_of.
func (r CostRate) AsOfDate() (time.Time, error) {
	return time.Parse(time.DateOnly, r.AsOf)
}

// validateCost checks every rate and normalizes currencies to upper case.
func (c *Config) validateCost() error {
	seen := map[string]bool{}
	for i := range c.Cost.Rates {
		r := &c.Cost.Rates[i]
		r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency))
		if err := r.validate(time.Now()); err != nil {
			return fmt.Errorf("cost.rates[%d]: %w", i, err)
		}
		key := "resource=" + r.Resource
		if r.Database != "" {
			key = "database=" + r.Database
		}
		if seen[key] {
			return fmt.Errorf("cost.rates[%d]: %s is priced more than once", i, key)
		}
		seen[key] = true
	}
	return nil
}

func (r CostRate) validate(now time.Time) error {
	if (r.Resource == "") == (r.Database == "") {
		return fmt.Errorf("name exactly one resource or database")
	}
	if !costCurrencies[r.Currency] {
		return fmt.Errorf("currency %q is not a supported ISO 4217 code", r.Currency)
	}
	at, err := r.AsOfDate()
	if err != nil {
		return fmt.Errorf("as_of %q must be a date (YYYY-MM-DD) the prices were read",
			r.AsOf)
	}
	if at.After(now.Add(24 * time.Hour)) {
		return fmt.Errorf("as_of %s is in the future", r.AsOf)
	}
	if err := r.validatePrices(); err != nil {
		return err
	}
	if r.VCPUs < 0 || r.VCPUs > maxCostVCPUs {
		return fmt.Errorf("vcpus must be 0-%d, got %d", maxCostVCPUs, r.VCPUs)
	}
	return nil
}

func (r CostRate) validatePrices() error {
	prices := []struct {
		name string
		v    *float64
	}{{"compute_per_hour", r.ComputePerHour}, {"dbu_per_hour", r.DBUPerHour},
		{"cu_per_hour", r.CUPerHour}, {"compute_units", &r.ComputeUnits},
		{"storage_gb_month", &r.StorageGBMonth}, {"iops_month", &r.IOPSMonth},
		{"extended_support_per_vcpu_hour", r.ExtendedSupportPerVCPUHour}}
	compute := 0
	for i, p := range prices {
		if p.v == nil {
			continue
		}
		if math.IsNaN(*p.v) || math.IsInf(*p.v, 0) || *p.v < 0 {
			return fmt.Errorf("%s must be a finite price >= 0, got %v", p.name, *p.v)
		}
		if i < 3 {
			compute++
		}
	}
	if compute != 1 {
		return fmt.Errorf("set exactly one compute price: compute_per_hour, dbu_per_hour " +
			"or cu_per_hour")
	}
	if (r.DBUPerHour != nil || r.CUPerHour != nil) && r.ComputeUnits <= 0 {
		return fmt.Errorf("dbu_per_hour and cu_per_hour need compute_units > 0")
	}
	if r.IOPSMonth > 0 && r.ProvisionedIOPS <= 0 {
		return fmt.Errorf("iops_month needs provisioned_iops > 0")
	}
	if r.ProvisionedIOPS < 0 {
		return fmt.Errorf("provisioned_iops must be >= 0")
	}
	return nil
}
