package config

import (
	"math"
	"strings"
	"testing"
)

// market-04: cost.rates are operator-supplied prices. A wrong dollar
// figure costs credibility, so every rate is finite, non-negative, in a
// known currency and dated; anything else fails the load.

const costRDS = `cost:
  rates:
    - resource: "rds:us-east-1/orders"
      currency: USD
      as_of: "2026-11-01"
      compute_per_hour: 0.504
      storage_gb_month: 0.115
`

func TestCost_DefaultIsOff(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Cost.Rates) != 0 || cfg.Cost.Configured() {
		t.Fatalf("cost = %+v, want no rates by default", cfg.Cost)
	}
}

func TestCost_LoadsRates(t *testing.T) {
	cfg, err := loadRCAYAML(t, costRDS)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Cost.Configured() || len(cfg.Cost.Rates) != 1 {
		t.Fatalf("rates = %+v", cfg.Cost.Rates)
	}
	r := cfg.Cost.Rates[0]
	if r.Resource != "rds:us-east-1/orders" || r.Currency != "USD" ||
		r.ComputePerHourValue() != 0.504 || r.StorageGBMonth != 0.115 {
		t.Fatalf("rate = %+v", r)
	}
	if at, err := r.AsOfDate(); err != nil || at.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("as_of = %v (%v)", at, err)
	}
}

func TestCost_CapacityUnitPricing(t *testing.T) {
	cfg, err := loadRCAYAML(t, `cost:
  rates:
    - database: lakebase-main
      currency: usd
      as_of: "2026-10-15"
      cu_per_hour: 0.40
      compute_units: 4
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Cost.Rates[0]
	if got := r.ComputePerHourValue(); math.Abs(got-1.6) > 1e-12 {
		t.Fatalf("compute per hour = %v, want 0.40 x 4 = 1.6", got)
	}
	if r.Currency != "USD" {
		t.Fatalf("currency = %q, want normalized USD", r.Currency)
	}
}

func TestCost_InvalidRatesFailTheLoad(t *testing.T) {
	head := "cost:\n  rates:\n    - resource: \"rds:us-east-1/orders\"\n"
	cases := map[string]struct{ body, want string }{
		"negative rate": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: -1\n", "compute_per_hour"},
		"NaN rate": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: .nan\n", "compute_per_hour"},
		"infinite storage": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: 1\n      storage_gb_month: .inf\n", "storage_gb_month"},
		"unknown currency": {"      currency: DOGE\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: 1\n", "currency"},
		"missing currency": {"      as_of: \"2026-11-01\"\n      compute_per_hour: 1\n",
			"currency"},
		"missing as_of": {"      currency: USD\n      compute_per_hour: 1\n", "as_of"},
		"bad as_of": {"      currency: USD\n      as_of: \"11/01/2026\"\n" +
			"      compute_per_hour: 1\n", "as_of"},
		"future as_of": {"      currency: USD\n      as_of: \"2999-01-01\"\n" +
			"      compute_per_hour: 1\n", "as_of"},
		"no compute price": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      storage_gb_month: 0.1\n", "compute"},
		"two compute prices": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: 1\n      cu_per_hour: 0.4\n      compute_units: 2\n",
			"compute"},
		"cu without units": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      cu_per_hour: 0.4\n", "compute_units"},
		"iops without provisioned": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: 1\n      iops_month: 0.1\n", "provisioned_iops"},
		"negative vcpus": {"      currency: USD\n      as_of: \"2026-11-01\"\n" +
			"      compute_per_hour: 1\n      vcpus: -2\n", "vcpus"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadRCAYAML(t, head+tc.body)
			if err == nil {
				t.Fatalf("load succeeded, want an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "cost.rates[0]") {
				t.Fatalf("error %q does not name cost.rates[0] and %s", err, tc.want)
			}
		})
	}
}

func TestCost_EntryNamesExactlyOneTarget(t *testing.T) {
	for name, target := range map[string]string{
		"neither": "",
		"both":    "      resource: \"rds:us-east-1/a\"\n      database: a\n",
	} {
		_, err := loadRCAYAML(t, "cost:\n  rates:\n    - currency: USD\n"+
			"      as_of: \"2026-11-01\"\n      compute_per_hour: 1\n"+target)
		if err == nil || !strings.Contains(err.Error(), "resource or database") {
			t.Fatalf("%s: err = %v, want resource-or-database error", name, err)
		}
	}
}

func TestCost_DuplicateTargetRefused(t *testing.T) {
	entry := "    - database: orders\n      currency: USD\n      as_of: \"2026-11-01\"\n" +
		"      compute_per_hour: 1\n"
	_, err := loadRCAYAML(t, "cost:\n  rates:\n"+entry+entry)
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("err = %v, want a duplicate error", err)
	}
}

func TestCost_ZeroRatesAreAllowed(t *testing.T) {
	// A free tier is a real price of zero, not a missing one.
	cfg, err := loadRCAYAML(t, "cost:\n  rates:\n    - database: free\n      currency: EUR\n"+
		"      as_of: \"2026-11-01\"\n      compute_per_hour: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cost.Rates[0].ComputePerHourValue() != 0 || !cfg.Cost.Rates[0].HasCompute() {
		t.Fatalf("rate = %+v, want an explicit zero compute price", cfg.Cost.Rates[0])
	}
}
