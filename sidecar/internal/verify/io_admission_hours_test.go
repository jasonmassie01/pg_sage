package verify

import "testing"

// Fast elevation: a learned baseline shorter than a day
// (verify.io_baseline_hours) is enforced at its boundary and reported in
// hours, so "learning IO baseline: 0.1/0.0833 days" never reaches a UI.

func TestSubDayBaselineIsEnforcedAndReportedInHours(t *testing.T) {
	options := admissionOptions()
	options.BaselineDays = 2.0 / 24
	for _, test := range []struct {
		observedHours float64
		ok            bool
		reason        string
		detail        string
	}{
		{0, false, ReasonLearningBaseline, "learning IO baseline: 0.0/2 hours"},
		{1.5, false, ReasonLearningBaseline, "learning IO baseline: 1.5/2 hours"},
		{1.99, false, ReasonLearningBaseline, "learning IO baseline: 1.9/2 hours"},
		{2, true, ReasonWithinBaseline, ""},
		{30, true, ReasonWithinBaseline, ""},
	} {
		got := DecideAdmission(LoadEvidence{
			CPUPct: floatPtr(10), Rate: ioRate(1, 1),
			Baseline: learnedBaseline(test.observedHours / 24),
		}, options)
		assertAdmission(t, got, test.ok, test.reason, EvidenceLearnedBaseline)
		if got.Detail != test.detail && test.detail != "" {
			t.Fatalf("%vh detail = %q, want %q", test.observedHours, got.Detail, test.detail)
		}
		if required, _ := got.Evidence["baseline_required_days"].(float64); required !=
			options.BaselineDays {
			t.Fatalf("evidence baseline_required_days = %v, want %v",
				got.Evidence["baseline_required_days"], options.BaselineDays)
		}
	}
}

// A baseline of a day or more keeps the day wording.
func TestDayScaleBaselineKeepsDays(t *testing.T) {
	options := admissionOptions()
	options.BaselineDays = 1
	got := DecideAdmission(LoadEvidence{
		CPUPct: floatPtr(10), Rate: ioRate(1, 1), Baseline: learnedBaseline(0.5),
	}, options)
	if got.Detail != "learning IO baseline: 0.5/1 days" {
		t.Fatalf("detail = %q", got.Detail)
	}
}
