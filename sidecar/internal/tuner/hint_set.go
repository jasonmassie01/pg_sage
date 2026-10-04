package tuner

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// setDirectiveRe matches Set(<guc> <value>) with an optionally quoted
// value.
var setDirectiveRe = regexp.MustCompile(
	`^Set\(\s*([A-Za-z_][A-Za-z0-9_.]*)\s+"?([^")]*?)"?\s*\)$`,
)

// workMemUnitMB converts PostgreSQL memory units to MB (case-sensitive).
var workMemUnitMB = map[string]float64{
	"kB": 1.0 / 1024, "MB": 1, "GB": 1024, "TB": 1024 * 1024,
}

var allowedPlanCacheModes = map[string]bool{
	"auto": true, "force_generic_plan": true, "force_custom_plan": true,
}

// maxParallelWorkersHint caps Set(max_parallel_workers_per_gather).
const maxParallelWorkersHint = 16

// normalizeSetDirectives rewrites every Set() directive in a hint
// into canonical form, rejecting GUCs outside the allowlist. Planner
// toggles, timeouts and other GUCs are refused: prompt-injected or
// hallucinated Set(statement_timeout "0") must never be installed
// (G3-B16). Non-Set directives pass through unchanged.
func normalizeSetDirectives(hint string, workMemMaxMB int) (string, error) {
	parts := splitHintDirectives(hint)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "Set(") {
			out = append(out, p)
			continue
		}
		norm, err := normalizeSet(p, workMemMaxMB)
		if err != nil {
			return "", err
		}
		out = append(out, norm)
	}
	return strings.Join(out, " "), nil
}

func normalizeSet(directive string, workMemMaxMB int) (string, error) {
	m := setDirectiveRe.FindStringSubmatch(directive)
	if m == nil {
		return "", fmt.Errorf("malformed Set directive %q", directive)
	}
	guc, value := strings.ToLower(m[1]), strings.TrimSpace(m[2])
	switch guc {
	case "work_mem":
		mb, err := parseWorkMemMB(value)
		if err != nil {
			return "", err
		}
		return fmtSetWorkMem(clampWorkMem(mb, workMemMaxMB)), nil
	case "plan_cache_mode":
		if !allowedPlanCacheModes[value] {
			return "", fmt.Errorf("plan_cache_mode %q not allowed", value)
		}
		return fmt.Sprintf(`Set(plan_cache_mode "%s")`, value), nil
	case "max_parallel_workers_per_gather":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > maxParallelWorkersHint {
			return "", fmt.Errorf("max_parallel_workers_per_gather %q out of range", value)
		}
		return fmt.Sprintf(`Set(max_parallel_workers_per_gather "%d")`, n), nil
	default:
		return "", fmt.Errorf("Set(%s) is not an allowed hint GUC", guc)
	}
}

// parseWorkMemMB parses a work_mem value; a bare number is kB (the GUC's
// base unit), units are case-sensitive as in PostgreSQL.
func parseWorkMemMB(value string) (int, error) {
	i := len(value)
	for i > 0 && (value[i-1] < '0' || value[i-1] > '9') {
		i--
	}
	num, unit := strings.TrimSpace(value[:i]), strings.TrimSpace(value[i:])
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid work_mem %q", value)
	}
	mult := 1.0 / 1024 // kB
	if unit != "" {
		m, ok := workMemUnitMB[unit]
		if !ok {
			return 0, fmt.Errorf("invalid work_mem unit %q", unit)
		}
		mult = m
	}
	mb := n * mult
	if mb > 1<<30 {
		mb = 1 << 30 // clamped to the ceiling by the caller
	}
	return int(mb), nil
}
