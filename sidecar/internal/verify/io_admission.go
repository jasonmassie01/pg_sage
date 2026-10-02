package verify

import (
	"fmt"
	"math"
	"strconv"
)

// Evidence modes recorded with every admission decision.
const (
	EvidenceDeclaredCapacity = "declared_capacity"
	EvidenceLearnedBaseline  = "learned_baseline"
	EvidenceUnavailable      = "unavailable"
)

// bytesPerMB converts declared MB/s. Provider tiers quote mebibytes.
const bytesPerMB = 1024 * 1024

// IOCapacity is the operator's attested provisioned throughput.
type IOCapacity struct {
	ReadWriteMBps float64
	WALMBps       float64
}

// LoadEvidence is everything load admission may consider. A nil CPUPct or
// Rate means that evidence is unavailable; it is never a zero reading.
type LoadEvidence struct {
	CPUPct     *float64
	Rate       *IORate
	RateError  string
	Capacity   *IOCapacity
	Baseline   IOBaseline
	WindowOpen bool
}

// DecideAdmission applies the earned-admission rules to the evidence:
// declared capacity (an operator attestation) wins over the learned
// baseline; without either, admission stays unavailable. Unknown CPU is
// admitted only inside a maintenance window.
func DecideAdmission(evidence LoadEvidence, options Options) Admission {
	record := admissionRecord(evidence, options)
	var admission Admission
	switch {
	case !validPercent(evidence.CPUPct) || !validRate(evidence.Rate):
		admission = Admission{
			Reason: ReasonLoadInvalid, Mode: evidenceMode(evidence, options),
			Detail: "load evidence must be finite, non-negative and CPU at most 100%",
		}
	case evidence.Rate == nil:
		admission = unavailableAdmission(rateUnavailableDetail(evidence.RateError))
	case evidence.Capacity != nil:
		admission = decideDeclared(evidence, options, record)
	case options.BaselineDays > 0:
		admission = decideBaseline(evidence, options)
	default:
		admission = unavailableAdmission(
			"no declared IO capacity and the learned IO baseline is disabled")
	}
	if admission.OK {
		admission = applyCPURule(admission, evidence, options)
	}
	record["evidence_mode"] = admission.Mode
	record["reason"] = admission.Reason
	admission.Evidence = record
	return admission
}

func evidenceMode(evidence LoadEvidence, options Options) string {
	switch {
	case evidence.Capacity != nil:
		return EvidenceDeclaredCapacity
	case options.BaselineDays > 0:
		return EvidenceLearnedBaseline
	default:
		return EvidenceUnavailable
	}
}

func validPercent(value *float64) bool {
	return value == nil || (validNumber(*value) && *value <= 100)
}

func validRate(rate *IORate) bool {
	return rate == nil || (validNumber(rate.DataBytesPerSec) && validNumber(rate.WALBytesPerSec))
}

func validNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func rateUnavailableDetail(rateError string) string {
	if rateError == "" {
		return "no fresh pg-side IO rate: the IO sampler is not running"
	}
	return "no fresh pg-side IO rate: " + rateError
}

// decideDeclared compares utilization of the attested capacity with the
// separate data and WAL IO ceilings. At exactly the ceiling is admitted.
func decideDeclared(evidence LoadEvidence, options Options, record map[string]any) Admission {
	capacity := *evidence.Capacity
	dataPct := utilizationPct(evidence.Rate.DataBytesPerSec, capacity.ReadWriteMBps)
	logPct := utilizationPct(evidence.Rate.WALBytesPerSec, capacity.WALMBps)
	record["data_io_pct"], record["log_io_pct"] = dataPct, logPct
	admission := Admission{Mode: EvidenceDeclaredCapacity}
	switch {
	case !validNumber(dataPct) || !validNumber(logPct) || dataPct > 100 || logPct > 100:
		admission.Reason = ReasonLoadInvalid
		admission.Detail = "measured IO exceeds the declared capacity; the attestation is inconsistent"
	case dataPct > options.DataIOCeilingPct:
		admission.Reason = ReasonDataIOCeiling
		admission.Detail = fmt.Sprintf("data IO at %.1f%% of declared capacity exceeds %.0f%%",
			dataPct, options.DataIOCeilingPct)
	case logPct > options.LogIOCeilingPct:
		admission.Reason = ReasonLogIOCeiling
		admission.Detail = fmt.Sprintf("WAL IO at %.1f%% of declared capacity exceeds %.0f%%",
			logPct, options.LogIOCeilingPct)
	default:
		admission.OK, admission.Reason = true, ReasonWithinCeiling
	}
	return admission
}

func utilizationPct(bytesPerSec, capacityMBps float64) float64 {
	if capacityMBps <= 0 {
		return math.NaN()
	}
	return bytesPerSec * 100 / (capacityMBps * bytesPerMB)
}

// decideBaseline admits only after BaselineDays of observation and only
// while current data and WAL rates are at or below the learned median.
func decideBaseline(evidence LoadEvidence, options Options) Admission {
	baseline := evidence.Baseline
	admission := Admission{Mode: EvidenceLearnedBaseline}
	switch {
	case baseline.ObservedDays < options.BaselineDays:
		admission.Reason = ReasonLearningBaseline
		admission.Detail = learningDetail(baseline.ObservedDays, options.BaselineDays)
	case evidence.Rate.DataBytesPerSec > baseline.DataP50:
		admission.Reason = ReasonDataIOAboveBaseline
		admission.Detail = "data IO is above the learned median; waiting for a quiet period"
	case evidence.Rate.WALBytesPerSec > baseline.WALP50:
		admission.Reason = ReasonWALAboveBaseline
		admission.Detail = "WAL is above the learned median; waiting for a quiet period"
	default:
		admission.OK, admission.Reason = true, ReasonWithinBaseline
	}
	return admission
}

// learningDetail reports baseline progress in days, or in hours for a
// sub-day baseline (verify.io_baseline_hours).
func learningDetail(observedDays, requiredDays float64) string {
	observed, required, unit := observedDays, requiredDays, "days"
	if requiredDays < 1 {
		observed, required, unit = observedDays*24, requiredDays*24, "hours"
	}
	return fmt.Sprintf("learning IO baseline: %.1f/%s %s",
		math.Floor(observed*10+1e-9)/10,
		strconv.FormatFloat(math.Round(required*100)/100, 'f', -1, 64), unit)
}

// applyCPURule withholds on measured CPU above the ceiling, and admits
// unknown CPU only inside a maintenance window.
func applyCPURule(admission Admission, evidence LoadEvidence, options Options) Admission {
	switch {
	case evidence.CPUPct != nil && *evidence.CPUPct > options.CPUCeilingPct:
		admission.OK, admission.Reason = false, ReasonCPUCeiling
		admission.Detail = fmt.Sprintf("host CPU %.1f%% exceeds %.0f%%",
			*evidence.CPUPct, options.CPUCeilingPct)
	case evidence.CPUPct == nil && !evidence.WindowOpen:
		admission.OK, admission.Reason = false, ReasonCPUUnknownWindowClose
		admission.Detail = "host CPU is unavailable; admitting only inside a maintenance window"
	}
	return admission
}

// admissionRecord is the decision evidence: which inputs decided.
func admissionRecord(evidence LoadEvidence, options Options) map[string]any {
	record := map[string]any{
		"window_open": evidence.WindowOpen, "cpu_evidence": "unavailable",
		"cpu_ceiling_pct":                 options.CPUCeilingPct,
		"baseline_required_days":          options.BaselineDays,
		"baseline_observed_days":          evidence.Baseline.ObservedDays,
		"baseline_samples":                evidence.Baseline.Samples,
		"baseline_data_p50_bytes_per_sec": evidence.Baseline.DataP50,
		"baseline_wal_p50_bytes_per_sec":  evidence.Baseline.WALP50,
	}
	if evidence.CPUPct != nil {
		record["cpu_evidence"], record["cpu_pct"] = "measured", *evidence.CPUPct
	}
	if rate := evidence.Rate; rate != nil {
		record["data_bytes_per_sec"] = rate.DataBytesPerSec
		record["wal_bytes_per_sec"] = rate.WALBytesPerSec
		record["rate_interval_seconds"] = rate.Interval.Seconds()
		record["io_source"] = rate.Source
	} else if evidence.RateError != "" {
		record["rate_error"] = evidence.RateError
	}
	if capacity := evidence.Capacity; capacity != nil {
		record["declared_read_write_mbps"] = capacity.ReadWriteMBps
		record["declared_wal_mbps"] = capacity.WALMBps
		record["data_io_ceiling_pct"] = options.DataIOCeilingPct
		record["log_io_ceiling_pct"] = options.LogIOCeilingPct
	}
	return record
}

// Admission reason codes.
const (
	ReasonLoadUnavailable       = "load_unavailable"
	ReasonLoadInvalid           = "load_invalid"
	ReasonWithinCeiling         = "load_within_ceiling"
	ReasonCPUCeiling            = "cpu_ceiling"
	ReasonDataIOCeiling         = "data_io_ceiling"
	ReasonLogIOCeiling          = "log_io_ceiling"
	ReasonLearningBaseline      = "learning_baseline"
	ReasonWithinBaseline        = "load_at_or_below_baseline"
	ReasonDataIOAboveBaseline   = "data_io_above_baseline"
	ReasonWALAboveBaseline      = "wal_above_baseline"
	ReasonCPUUnknownWindowClose = "cpu_unavailable_outside_window"
)

func unavailableAdmission(detail string) Admission {
	return Admission{
		Reason: ReasonLoadUnavailable, Mode: EvidenceUnavailable, Detail: detail,
	}
}
