package verify

import (
	"math"
	"strings"
	"testing"
	"time"
)

const mib = 1024 * 1024

func floatPtr(value float64) *float64 { return &value }

func admissionOptions() Options {
	options := DefaultOptions()
	options.CPUCeilingPct, options.DataIOCeilingPct, options.LogIOCeilingPct = 90, 60, 50
	options.BaselineDays = 7
	return options
}

func ioRate(dataMBps, walMBps float64) *IORate {
	return &IORate{
		At: ioTestStart, Interval: time.Minute, Source: IOSourcePGStatIO,
		DataBytesPerSec: dataMBps * mib, WALBytesPerSec: walMBps * mib,
	}
}

func learnedBaseline(days float64) IOBaseline {
	return IOBaseline{ObservedDays: days, Samples: 10080, DataP50: 40 * mib, WALP50: 4 * mib}
}

func assertAdmission(t *testing.T, got Admission, ok bool, reason, mode string) {
	t.Helper()
	if got.OK != ok || got.Reason != reason || got.Mode != mode {
		t.Fatalf("admission = {OK:%v Reason:%q Mode:%q Detail:%q}, want %v/%q/%q",
			got.OK, got.Reason, got.Mode, got.Detail, ok, reason, mode)
	}
}

func TestPGRateLoadDerivesUtilizationFromDeclaredCapacity(t *testing.T) {
	evidence := LoadEvidence{
		CPUPct: floatPtr(25), Rate: ioRate(250, 20),
		Capacity: &IOCapacity{ReadWriteMBps: 1000, WALMBps: 100},
	}
	got := DecideAdmission(evidence, admissionOptions())
	assertAdmission(t, got, true, ReasonWithinCeiling, EvidenceDeclaredCapacity)
	if got.Evidence["data_io_pct"] != 25.0 || got.Evidence["log_io_pct"] != 20.0 {
		t.Fatalf("utilization evidence = %#v, want 25%% data and 20%% WAL", got.Evidence)
	}
	if got.Evidence["declared_read_write_mbps"] != 1000.0 ||
		got.Evidence["declared_wal_mbps"] != 100.0 {
		t.Fatalf("attestation missing from evidence: %#v", got.Evidence)
	}
	if got.Evidence["evidence_mode"] != EvidenceDeclaredCapacity {
		t.Fatalf("evidence mode = %#v", got.Evidence["evidence_mode"])
	}
}

func TestDeclaredCapacityAtExactlyCeilingAdmitsAboveWithholds(t *testing.T) {
	capacity := &IOCapacity{ReadWriteMBps: 1000, WALMBps: 100}
	cases := []struct {
		name   string
		rate   *IORate
		ok     bool
		reason string
	}{
		{"both exactly at ceiling", ioRate(600, 50), true, ReasonWithinCeiling},
		{"data just above", ioRate(600.001, 50), false, ReasonDataIOCeiling},
		{"wal just above", ioRate(600, 50.001), false, ReasonLogIOCeiling},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := DecideAdmission(LoadEvidence{
				CPUPct: floatPtr(90), Rate: test.rate, Capacity: capacity,
			}, admissionOptions())
			assertAdmission(t, got, test.ok, test.reason, EvidenceDeclaredCapacity)
		})
	}
}

func TestDeclaredCapacityOverridesLearningBaseline(t *testing.T) {
	got := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(10), Rate: ioRate(900, 10), Baseline: learnedBaseline(1),
		Capacity: &IOCapacity{ReadWriteMBps: 1000, WALMBps: 100},
	}, admissionOptions())
	// 90% data IO exceeds the 60% ceiling even though the baseline would
	// otherwise still be learning: the attestation decides.
	assertAdmission(t, got, false, ReasonDataIOCeiling, EvidenceDeclaredCapacity)
}

func TestDeclaredCapacityRejectsUtilizationAboveAttestation(t *testing.T) {
	got := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(10), Rate: ioRate(1001, 1),
		Capacity: &IOCapacity{ReadWriteMBps: 1000, WALMBps: 100},
	}, admissionOptions())
	assertAdmission(t, got, false, ReasonLoadInvalid, EvidenceDeclaredCapacity)
}

func TestLearnedBaselineRequiresSevenDays(t *testing.T) {
	for _, test := range []struct {
		days   float64
		ok     bool
		reason string
		detail string
	}{
		{6.9, false, ReasonLearningBaseline, "learning IO baseline: 6.9/7 days"},
		{7, true, ReasonWithinBaseline, ""},
		{3, false, ReasonLearningBaseline, "learning IO baseline: 3.0/7 days"},
	} {
		got := DecideAdmission(LoadEvidence{
			CPUPct: floatPtr(10), Rate: ioRate(1, 1), Baseline: learnedBaseline(test.days),
		}, admissionOptions())
		assertAdmission(t, got, test.ok, test.reason, EvidenceLearnedBaseline)
		if test.detail != "" && got.Detail != test.detail {
			t.Fatalf("%v days detail = %q, want %q", test.days, got.Detail, test.detail)
		}
	}
}

func TestLearnedBaselineMedianBoundary(t *testing.T) {
	baseline := learnedBaseline(7)
	cases := []struct {
		name   string
		rate   *IORate
		ok     bool
		reason string
	}{
		{"exactly at median", ioRate(40, 4), true, ReasonWithinBaseline},
		{"data above median", ioRate(40.0001, 4), false, ReasonDataIOAboveBaseline},
		{"wal above median", ioRate(40, 4.0001), false, ReasonWALAboveBaseline},
		{"idle", ioRate(0, 0), true, ReasonWithinBaseline},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := DecideAdmission(LoadEvidence{
				CPUPct: floatPtr(5), Rate: test.rate, Baseline: baseline,
			}, admissionOptions())
			assertAdmission(t, got, test.ok, test.reason, EvidenceLearnedBaseline)
			if got.Evidence["baseline_data_p50_bytes_per_sec"] != 40.0*mib {
				t.Fatalf("baseline median missing from evidence: %#v", got.Evidence)
			}
		})
	}
}

func TestCPUUnavailableRequiresOpenWindow(t *testing.T) {
	for _, mode := range []string{EvidenceLearnedBaseline, EvidenceDeclaredCapacity} {
		evidence := LoadEvidence{Rate: ioRate(1, 1), Baseline: learnedBaseline(8)}
		if mode == EvidenceDeclaredCapacity {
			evidence.Capacity = &IOCapacity{ReadWriteMBps: 1000, WALMBps: 100}
		}
		closed := DecideAdmission(evidence, admissionOptions())
		assertAdmission(t, closed, false, ReasonCPUUnknownWindowClose, mode)
		evidence.WindowOpen = true
		open := DecideAdmission(evidence, admissionOptions())
		if !open.OK || open.Mode != mode || open.Evidence["cpu_evidence"] != "unavailable" {
			t.Fatalf("%s with open window = %+v", mode, open)
		}
	}
}

func TestMeasuredCPUAboveCeilingWithholdsEvenInWindow(t *testing.T) {
	got := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(90.5), Rate: ioRate(1, 1), Baseline: learnedBaseline(8),
		WindowOpen: true,
	}, admissionOptions())
	assertAdmission(t, got, false, ReasonCPUCeiling, EvidenceLearnedBaseline)
	atCeiling := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(90), Rate: ioRate(1, 1), Baseline: learnedBaseline(8),
	}, admissionOptions())
	assertAdmission(t, atCeiling, true, ReasonWithinBaseline, EvidenceLearnedBaseline)
}

func TestAdmissionWithoutRateIsUnavailable(t *testing.T) {
	got := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(1), RateError: "IO statistics were reset", WindowOpen: true,
		Baseline: learnedBaseline(30),
		Capacity: &IOCapacity{ReadWriteMBps: 1000, WALMBps: 100},
	}, admissionOptions())
	assertAdmission(t, got, false, ReasonLoadUnavailable, EvidenceUnavailable)
	if !strings.Contains(got.Detail, "reset") {
		t.Fatalf("detail %q hides why the rate is unavailable", got.Detail)
	}
	empty := DecideAdmission(LoadEvidence{}, admissionOptions())
	assertAdmission(t, empty, false, ReasonLoadUnavailable, EvidenceUnavailable)
}

func TestAdmissionUnchangedWithoutIOCapacity(t *testing.T) {
	options := admissionOptions()
	options.BaselineDays = 0
	got := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(1), Rate: ioRate(0, 0), Baseline: learnedBaseline(365),
		WindowOpen: true,
	}, options)
	assertAdmission(t, got, false, ReasonLoadUnavailable, EvidenceUnavailable)
}

func TestAdmissionRejectsNonfiniteEvidence(t *testing.T) {
	bad := []LoadEvidence{
		{CPUPct: floatPtr(math.NaN()), Rate: ioRate(1, 1), Baseline: learnedBaseline(8)},
		{CPUPct: floatPtr(101), Rate: ioRate(1, 1), Baseline: learnedBaseline(8)},
		{CPUPct: floatPtr(1), Rate: ioRate(-1, 1), Baseline: learnedBaseline(8)},
		{CPUPct: floatPtr(1), Rate: ioRate(math.Inf(1), 1), Baseline: learnedBaseline(8)},
		{CPUPct: floatPtr(1), Rate: ioRate(1, math.NaN()), Baseline: learnedBaseline(8)},
	}
	for i, evidence := range bad {
		got := DecideAdmission(evidence, admissionOptions())
		if got.OK || got.Reason != ReasonLoadInvalid {
			t.Fatalf("bad evidence %d admitted: %+v", i, got)
		}
	}
}

func TestDefaultOptionsRequireSevenDayBaseline(t *testing.T) {
	options := DefaultOptions()
	if options.BaselineDays != 7 {
		t.Fatalf("default baseline days = %v, want 7", options.BaselineDays)
	}
	if options.DataIOCeilingPct != 70 || options.LogIOCeilingPct != 70 {
		t.Fatalf("default IO ceilings = %v/%v, want 70/70",
			options.DataIOCeilingPct, options.LogIOCeilingPct)
	}
}
