package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// floorRule is one line of the floors file.
type floorRule struct {
	Line    int
	Pattern *regexp.Regexp
	Floor   float64
	Reason  string
}

// parseFloors reads `<package-regex> <floor> # reason` lines. Blank lines and
// lines starting with '#' are comments. Every rule needs a reason, so a
// lowered floor always says why; patterns are anchored to the whole package
// path relative to the module root.
func parseFloors(r io.Reader) ([]floorRule, error) {
	var rules []floorRule
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		rule, err := parseFloorLine(text)
		if err != nil {
			return nil, fmt.Errorf("floors line %d (%s): %w", line, text, err)
		}
		rule.Line = line
		rules = append(rules, rule)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the floors file: %w", err)
	}
	return rules, nil
}

func parseFloorLine(text string) (floorRule, error) {
	spec, reason, _ := strings.Cut(text, "#")
	reason = strings.TrimSpace(reason)
	fields := strings.Fields(spec)
	if len(fields) != 2 {
		return floorRule{}, fmt.Errorf("want `<package-regex> <floor> # reason`, got %d "+
			"fields before the '#'", len(fields))
	}
	if reason == "" {
		return floorRule{}, fmt.Errorf("no reason: every floor needs `# reason`")
	}
	floor, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || math.IsNaN(floor) {
		return floorRule{}, fmt.Errorf("floor %q is not a number", fields[1])
	}
	if floor < 0 || floor > 100 {
		return floorRule{}, fmt.Errorf("floor %v is outside 0 to 100", floor)
	}
	pattern, err := regexp.Compile(`^(?:` + fields[0] + `)$`)
	if err != nil {
		return floorRule{}, fmt.Errorf("package regex does not compile: %w", err)
	}
	return floorRule{Pattern: pattern, Floor: floor, Reason: reason}, nil
}

// floorFor returns the floor of pkg (relative to the module root) and the
// rule that set it, or def and nil when no rule matches.
func floorFor(rules []floorRule, def float64, pkg string) (float64, *floorRule) {
	for i := range rules {
		if rules[i].Pattern.MatchString(pkg) {
			return rules[i].Floor, &rules[i]
		}
	}
	return def, nil
}
