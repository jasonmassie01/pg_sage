package approvalcard

import "fmt"

// calibrationText renders the tuning agent's confidence calibration
// (detail "confidence_calibration"): how many comparable actions the
// outcome ledger saw improve, or that there is not enough history yet.
func calibrationText(raw any) string {
	cal, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	n, _ := number(cal["n"])
	switch cal["status"] {
	case "uncalibrated":
		if need, ok := number(cal["min_outcomes"]); ok {
			return fmt.Sprintf("uncalibrated (%.0f of %.0f outcomes)", n, need)
		}
		return "uncalibrated"
	case "calibrated":
		hits, _ := number(cal["hits"])
		scope := "all predictions of this kind"
		if basis, _ := cal["basis"].(string); basis == "bin" {
			bin, _ := cal["bin"].(string)
			scope = bin + "% predicted"
		}
		return fmt.Sprintf("%.0f of %.0f comparable actions improved (%s)", hits, n, scope)
	}
	return ""
}

// lookalikeText renders the fleet's look-alike prior (detail
// "lookalike_prior"): how comparable actions went on similar databases.
func lookalikeText(raw any) string {
	p, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	n, okN := number(p["outcomes"])
	dbs, okD := number(p["databases"])
	if !okN || !okD || n <= 0 || dbs <= 0 {
		return ""
	}
	improved, _ := number(p["improved"])
	regressed, _ := number(p["regressed"])
	text := fmt.Sprintf("%.0f of %.0f comparable actions improved", improved, n)
	if regressed > 0 {
		text += fmt.Sprintf(", %.0f regressed,", regressed)
	}
	noun := "databases"
	if dbs == 1 {
		noun = "database"
	}
	text += fmt.Sprintf(" on %.0f look-alike %s", dbs, noun)
	switch p["match"] {
	case "table_shape":
		text += " (same table shape)"
	case "action_class":
		text += " (same kind of action)"
	}
	return text
}
