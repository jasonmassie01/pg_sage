package optimizer

import (
	"math"
	"testing"
)

func approxEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

func TestComputeConfidence_AllSignalsMax(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      1.0,
		PlanClarity:      1.0,
		WriteRateKnown:   1.0,
		HypoPGValidated:  1.0,
		SelectivityKnown: 1.0,
		TableCallVolume:  1.0,
	}
	got := ComputeConfidence(input)
	if !approxEqual(got, 1.0, 0.001) {
		t.Errorf("all max: got %.4f, want 1.0", got)
	}
}

func TestComputeConfidence_AllSignalsZero(t *testing.T) {
	got := ComputeConfidence(ConfidenceInput{})
	if got != 0.0 {
		t.Errorf("all zero: got %.4f, want 0.0", got)
	}
}

func TestComputeConfidence_HighConfidence_NoHypoPG(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      1.0,
		PlanClarity:      1.0,
		WriteRateKnown:   1.0,
		HypoPGValidated:  0.0,
		SelectivityKnown: 1.0,
		TableCallVolume:  1.0,
	}
	got := ComputeConfidence(input)
	// 0.25 + 0.25 + 0.15 + 0.0 + 0.10 + 0.10 = 0.85
	if !approxEqual(got, 0.85, 0.001) {
		t.Errorf("high no-hypopg: got %.4f, want 0.85", got)
	}
}

func TestComputeConfidence_MediumConfidence_NoHypoPG(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      0.7,
		PlanClarity:      0.5,
		WriteRateKnown:   1.0,
		HypoPGValidated:  0.0,
		SelectivityKnown: 0.5,
		TableCallVolume:  0.6,
	}
	got := ComputeConfidence(input)
	// 0.175 + 0.125 + 0.15 + 0.0 + 0.05 + 0.06 = 0.56
	if !approxEqual(got, 0.56, 0.001) {
		t.Errorf("medium no-hypopg: got %.4f, want 0.56", got)
	}
}

func TestComputeConfidence_LowConfidence_ColdStart(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      0.4,
		PlanClarity:      0.0,
		WriteRateKnown:   0.0,
		HypoPGValidated:  0.0,
		SelectivityKnown: 0.0,
		TableCallVolume:  0.3,
	}
	got := ComputeConfidence(input)
	// 0.10 + 0.0 + 0.0 + 0.0 + 0.0 + 0.03 = 0.13
	if !approxEqual(got, 0.13, 0.001) {
		t.Errorf("cold start: got %.4f, want 0.13", got)
	}
}

func TestComputeConfidence_WithHypoPG_Boost(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      0.7,
		PlanClarity:      0.5,
		WriteRateKnown:   1.0,
		HypoPGValidated:  1.0,
		SelectivityKnown: 0.5,
		TableCallVolume:  0.6,
	}
	got := ComputeConfidence(input)
	// 0.175 + 0.125 + 0.15 + 0.15 + 0.05 + 0.06 = 0.71
	if !approxEqual(got, 0.71, 0.001) {
		t.Errorf("with hypopg: got %.4f, want 0.71", got)
	}
}

func TestComputeConfidence_HypoPGNoImprovement(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      0.7,
		PlanClarity:      0.5,
		WriteRateKnown:   1.0,
		HypoPGValidated:  0.2,
		SelectivityKnown: 0.5,
		TableCallVolume:  0.6,
	}
	got := ComputeConfidence(input)
	// 0.175 + 0.125 + 0.15 + 0.03 + 0.05 + 0.06 = 0.59
	if !approxEqual(got, 0.59, 0.001) {
		t.Errorf("hypopg no improvement: got %.4f, want 0.59", got)
	}
}

func TestComputeConfidence_QueryVolumeOnly(t *testing.T) {
	input := ConfidenceInput{QueryVolume: 1.0}
	got := ComputeConfidence(input)
	if !approxEqual(got, 0.25, 0.001) {
		t.Errorf("query volume only: got %.4f, want 0.25", got)
	}
}

func TestComputeConfidence_PlanClarityOnly(t *testing.T) {
	input := ConfidenceInput{PlanClarity: 1.0}
	got := ComputeConfidence(input)
	if !approxEqual(got, 0.25, 0.001) {
		t.Errorf("plan clarity only: got %.4f, want 0.25", got)
	}
}

func TestComputeConfidence_CappedAt1(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      1.5,
		PlanClarity:      1.5,
		WriteRateKnown:   1.5,
		HypoPGValidated:  1.5,
		SelectivityKnown: 1.5,
		TableCallVolume:  1.5,
	}
	got := ComputeConfidence(input)
	if got > 1.0 {
		t.Errorf("should be capped at 1.0, got %.4f", got)
	}
}

// --- ActionLevel ---

func TestActionLevel_Safe(t *testing.T) {
	if got := ActionLevel(0.7); got != "safe" {
		t.Errorf("ActionLevel(0.7) = %q, want safe", got)
	}
	if got := ActionLevel(1.0); got != "safe" {
		t.Errorf("ActionLevel(1.0) = %q, want safe", got)
	}
}

func TestActionLevel_Moderate(t *testing.T) {
	if got := ActionLevel(0.4); got != "moderate" {
		t.Errorf("ActionLevel(0.4) = %q, want moderate", got)
	}
	if got := ActionLevel(0.69); got != "moderate" {
		t.Errorf("ActionLevel(0.69) = %q, want moderate", got)
	}
}

func TestActionLevel_HighRisk(t *testing.T) {
	if got := ActionLevel(0.39); got != "high_risk" {
		t.Errorf("ActionLevel(0.39) = %q, want high_risk", got)
	}
	if got := ActionLevel(0.0); got != "high_risk" {
		t.Errorf("ActionLevel(0.0) = %q, want high_risk", got)
	}
}

func TestActionLevel_BoundaryExact(t *testing.T) {
	// Exactly at boundary
	if got := ActionLevel(0.4); got != "moderate" {
		t.Errorf("ActionLevel(0.4) = %q, want moderate", got)
	}
	if got := ActionLevel(0.7); got != "safe" {
		t.Errorf("ActionLevel(0.7) = %q, want safe", got)
	}
}

func TestComputeConfidence_CloudSQLTypical(t *testing.T) {
	// Simulates Cloud SQL PG16: high-traffic table, GENERIC_PLAN available,
	// write rate known, no HypoPG, pg_stats available.
	input := ConfidenceInput{
		QueryVolume:      1.0,  // 500+ calls
		PlanClarity:      1.0,  // GENERIC_PLAN available
		WriteRateKnown:   1.0,  // multiple snapshots
		HypoPGValidated:  0.0,  // unavailable on Cloud SQL
		SelectivityKnown: 1.0,  // pg_stats with n_distinct + MCV
		TableCallVolume:  1.0,  // 1000+ total calls
	}
	got := ComputeConfidence(input)
	if got < 0.8 {
		t.Errorf("Cloud SQL typical: got %.4f, want >= 0.8", got)
	}
	level := ActionLevel(got)
	if level != "safe" {
		t.Errorf("Cloud SQL typical: level = %q, want safe", level)
	}
}

func TestComputeConfidence_CloudSQLMedium(t *testing.T) {
	// Medium-traffic table on Cloud SQL, query text only (no plans).
	input := ConfidenceInput{
		QueryVolume:      0.7,  // 100-499 calls
		PlanClarity:      0.5,  // query text only
		WriteRateKnown:   1.0,
		HypoPGValidated:  0.0,
		SelectivityKnown: 0.5,  // n_distinct only
		TableCallVolume:  0.6,  // 100-999 calls
	}
	got := ComputeConfidence(input)
	if got < 0.4 || got > 0.7 {
		t.Errorf("Cloud SQL medium: got %.4f, want 0.4-0.7 range", got)
	}
	level := ActionLevel(got)
	if level != "moderate" {
		t.Errorf("Cloud SQL medium: level = %q, want moderate", level)
	}
}
