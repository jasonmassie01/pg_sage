package perfgate

import (
	"fmt"
	"strings"
)

// RenderCalibratedMarkdown is RenderMarkdown with the runner calibration
// the budgets (already calibrated) were scaled by, after the budgets.
func RenderCalibratedMarkdown(s Scale, b Budgets, c Calibration, phases []Phase,
	offenders []Offender) string {
	md := RenderMarkdown(s, b, phases, offenders)
	section := calibrationSection(c)
	if i := strings.Index(md, "\n## Offenders"); i >= 0 {
		return md[:i] + section + md[i:]
	}
	return md + section
}

func calibrationSection(c Calibration) string {
	var sb strings.Builder
	sb.WriteString("\n## Runner calibration\n\n")
	if !c.Known {
		sb.WriteString("Not measured: the budgets above are the shipped ones.\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "The timing budgets above are the shipped ones scaled by this runner's "+
		"speed against the reference runner (never below x1, at most x%.2f).\n\n",
		MaxCalibrationFactor)
	sb.WriteString("| Workload | This runner | Reference | Factor |\n|---|---|---|---|\n")
	fmt.Fprintf(&sb, "| CPU workload (sidecar CPU) | %.1f ms | %.1f ms | x%.2f |\n",
		c.CPUMs, ReferenceCPUMs, c.CPUFactor)
	fmt.Fprintf(&sb, "| SQL workload (statement, cycle, catalog and endpoint times) | "+
		"%.1f ms | %.1f ms | x%.2f |\n", c.DBMs, ReferenceDBMs, c.DBFactor)
	return sb.String()
}
