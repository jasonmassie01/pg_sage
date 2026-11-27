package config

import (
	"strings"
	"testing"
)

// drill-02a: cost.rates[].extended_support_per_vcpu_hour is the
// operator's extended-support price; optional, and when set finite and
// non-negative like every other price. No concurrency or DB test: config
// validation is a pure function of the loaded file.

func TestCost_ExtendedSupportRateIsOptional(t *testing.T) {
	cfg, err := loadRCAYAML(t, costRDS)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r := cfg.Cost.Rates[0]; r.ExtendedSupportPerVCPUHour != nil {
		t.Fatalf("extended support = %v, want unset by default", *r.ExtendedSupportPerVCPUHour)
	}
}

func TestCost_ExtendedSupportRateLoads(t *testing.T) {
	cfg, err := loadRCAYAML(t, costRDS+"      extended_support_per_vcpu_hour: 0.1\n"+
		"      vcpus: 4\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Cost.Rates[0]
	if r.ExtendedSupportPerVCPUHour == nil || *r.ExtendedSupportPerVCPUHour != 0.1 ||
		r.VCPUs != 4 {
		t.Fatalf("rate = %+v, want 0.1 per vCPU-hour on 4 vCPUs", r)
	}
	if r.ComputePerHourValue() != 0.504 {
		t.Fatalf("compute = %v: the extended-support price is not a compute price",
			r.ComputePerHourValue())
	}
}

func TestCost_ExtendedSupportRateZeroIsAllowed(t *testing.T) {
	cfg, err := loadRCAYAML(t, costRDS+"      extended_support_per_vcpu_hour: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p := cfg.Cost.Rates[0].ExtendedSupportPerVCPUHour; p == nil || *p != 0 {
		t.Fatalf("zero rate = %v, want an explicit 0", p)
	}
}

func TestCost_InvalidExtendedSupportRateFailsTheLoad(t *testing.T) {
	for _, v := range []string{"-0.1", ".nan", ".inf"} {
		_, err := loadRCAYAML(t, costRDS+"      extended_support_per_vcpu_hour: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "extended_support_per_vcpu_hour") ||
			!strings.Contains(err.Error(), "cost.rates[0]") {
			t.Fatalf("value %s: err = %v, want cost.rates[0] naming the field", v, err)
		}
	}
}
