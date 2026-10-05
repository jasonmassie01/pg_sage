package approvalcard

import (
	"strings"
	"testing"
)

// Approval cards show the tuning agent's calibrated confidence (roadmap
// 2.2): the share of comparable past actions the outcome ledger saw
// improve, or "uncalibrated" when there is not enough history. A card
// never shows an invented confidence.

func agentCard(detail map[string]any) Card {
	a := queued(9, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON public.orders (customer_id)",
		"DROP INDEX CONCURRENTLY public.i", "moderate")
	return Assemble(Inputs{Database: "orders", Action: a, Now: now,
		Finding: &FindingRow{ID: a.FindingID, Category: "missing_index",
			Severity: "info", ObjectType: "index", Object: "public.orders|btree(customer_id)",
			Title: "Index recommendation for public.orders", Detail: detail}})
}

func TestCardUncalibratedConfidence(t *testing.T) {
	c := agentCard(map[string]any{"llm_rationale": "seq scans on customer_id",
		"producer": "tuning_agent",
		"confidence_calibration": map[string]any{"status": "uncalibrated", "n": float64(2),
			"min_outcomes": float64(5)}})
	r := c.Rationale
	if r == nil || r.Confidence != nil || r.Calibration != "uncalibrated (2 of 5 outcomes)" {
		t.Fatalf("rationale = %+v", r)
	}
	text := Text(c, now)
	if !strings.Contains(text, "Model rationale (uncalibrated (2 of 5 outcomes))") ||
		strings.Contains(text, "confidence") {
		t.Fatalf("text:\n%s", text)
	}
}

func TestCardCalibratedConfidence(t *testing.T) {
	c := agentCard(map[string]any{"llm_rationale": "seq scans on customer_id",
		"producer": "tuning_agent", "confidence_score": 0.8,
		"confidence_calibration": map[string]any{"status": "calibrated", "n": float64(10),
			"hits": float64(8), "basis": "bin", "bin": "25-50"}})
	r := c.Rationale
	if r == nil || r.Confidence == nil || *r.Confidence != 0.8 ||
		r.Calibration != "8 of 10 comparable actions improved (25-50% predicted)" {
		t.Fatalf("rationale = %+v", r)
	}
	text := Text(c, now)
	if !strings.Contains(text, "calibrated confidence 80%: 8 of 10 comparable actions "+
		"improved (25-50% predicted)") {
		t.Fatalf("text:\n%s", text)
	}
}

func TestCardPooledCalibrationNamesTheClass(t *testing.T) {
	c := agentCard(map[string]any{"llm_rationale": "x", "confidence_score": 0.5,
		"confidence_calibration": map[string]any{"status": "calibrated", "n": float64(6),
			"hits": float64(3), "basis": "class_method", "bin": "50+"}})
	if got := c.Rationale.Calibration; got != "3 of 6 comparable actions improved "+
		"(all predictions of this kind)" {
		t.Fatalf("calibration = %q", got)
	}
}

func TestCardLegacyConfidenceUnchanged(t *testing.T) {
	c := agentCard(map[string]any{"llm_rationale": "x", "confidence_score": 0.82})
	if c.Rationale.Calibration != "" || *c.Rationale.Confidence != 0.82 {
		t.Fatalf("rationale = %+v", c.Rationale)
	}
	if text := Text(c, now); !strings.Contains(text, "(confidence 82%)") {
		t.Fatalf("text:\n%s", text)
	}
}

func TestCardCalibrationIsNotPartOfTheHash(t *testing.T) {
	a := agentCard(map[string]any{"llm_rationale": "x"})
	b := agentCard(map[string]any{"llm_rationale": "x",
		"confidence_calibration": map[string]any{"status": "uncalibrated"}})
	if a.CardHash != b.CardHash {
		t.Fatal("the card hash binds the action, not the evolving calibration")
	}
}
