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

// calibrationSection says what the runner is, what it measured and which
// budgets each factor scaled.
func calibrationSection(c Calibration) string {
	var sb strings.Builder
	sb.WriteString("\n## Runner calibration\n\n")
	if !c.Known {
		sb.WriteString("Not measured: the budgets above are the shipped ones.\n")
		return sb.String()
	}
	model := c.CPUModel
	if model == "" {
		model = "unknown CPU model"
	}
	fmt.Fprintf(&sb, "Runner: %s, %d CPUs. Each workload's time is the best of %d runs. "+
		"The timing budgets above are the shipped ones scaled by this runner's time over "+
		"the reference runner's (the median of %d reference runs), clamped to "+
		"x%.2f-x%.2f: tighter on a faster runner, looser on a slower one.\n\n", model,
		c.CPUs, calibrationRuns, len(referenceRuns), MinCalibrationFactor,
		MaxCalibrationFactor)
	sb.WriteString("| Workload | This runner | Reference | Factor | Scales |\n" +
		"|---|---|---|---|---|\n")
	fmt.Fprintf(&sb, "| CPU workload | %.1f ms | %.1f ms | x%.2f | %s |\n", c.CPUMs,
		ReferenceCPUMs, c.CPUFactor, GateSidecarCPU)
	fmt.Fprintf(&sb, "| SQL workload | %.1f ms | %.1f ms | x%.2f | %s and its exemption "+
		"ceilings, %s, %s |\n", c.DBMs, ReferenceDBMs, c.DBFactor, GateStatementMean,
		GateCycleDBTime, GateCatalogMax)
	fmt.Fprintf(&sb, "| larger of the two | | | x%.2f | %s |\n", c.EndpointFactor(),
		GateEndpoint)
	return sb.String()
}
