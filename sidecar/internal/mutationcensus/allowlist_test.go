package mutationcensus

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
)

// entry is one sinks.tsv row.
type entry struct {
	key, class, note string
	line             int
}

var (
	keyShape    = regexp.MustCompile(`^(?:internal|cmd)/[A-Za-z0-9_./-]+\.go#[A-Za-z0-9_.:]+$`)
	reasonShape = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// parseSinks reads sinks.tsv: path#Func<TAB>class<TAB>note per line;
// blank lines and lines starting with "#" are ignored.
func parseSinks(r io.Reader) ([]entry, error) {
	var out []entry
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("sinks.tsv line %d: want path#Func<TAB>class<TAB>note, "+
				"got %d tab-separated fields", n, len(fields))
		}
		e := entry{key: fields[0], class: fields[1], note: strings.TrimSpace(fields[2]), line: n}
		if !keyShape.MatchString(e.key) {
			return nil, fmt.Errorf("sinks.tsv line %d: key %q is not internal/...go#Func "+
				"or cmd/...go#Func", n, e.key)
		}
		if e.note == "" {
			return nil, fmt.Errorf("sinks.tsv line %d: %s has no note saying why", n, e.key)
		}
		if first, dup := seen[e.key]; dup {
			return nil, fmt.Errorf("sinks.tsv line %d: %s is already listed on line %d",
				n, e.key, first)
		}
		seen[e.key] = n
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read sinks.tsv: %w", err)
	}
	return out, nil
}

// checkClass validates one class. A gate class must name contracts the
// executor registers (ContractForActionType), so a typo or a removed
// contract fails here.
func checkClass(class string) error {
	kind, detail, hasDetail := strings.Cut(class, ":")
	switch kind {
	case "sage_schema", "text", "clone_only":
		if hasDetail {
			return fmt.Errorf("class %q takes no detail", class)
		}
		return nil
	case "out_of_gate":
		if !reasonShape.MatchString(detail) {
			return fmt.Errorf("class %q needs a snake_case reason: out_of_gate:<reason>", class)
		}
		return nil
	case "gate":
		if detail == "" {
			return fmt.Errorf("class %q names no contract: gate:<contract>[,<contract>]", class)
		}
		for _, c := range strings.Split(detail, ",") {
			if _, ok := executor.ContractForActionType(c); !ok {
				return fmt.Errorf("class %q: %q is not a contract registered in "+
					"internal/executor (ContractForActionType)", class, c)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown class %q: want gate:<contract>, sage_schema, text, "+
		"clone_only or out_of_gate:<reason>", class)
}

// compareCensus returns every disagreement between the sinks found and the
// listed entries: unlisted sinks, stale rows and bad classes.
func compareCensus(found []sink, entries []entry) []string {
	listed := map[string]entry{}
	for _, e := range entries {
		listed[e.key] = e
	}
	seen := map[string]bool{}
	var problems []string
	for _, s := range found {
		seen[s.key] = true
		if _, ok := listed[s.key]; !ok {
			problems = append(problems, fmt.Sprintf("unclassified sink %s (rule %s: %s): "+
				"add a sinks.tsv row with its class", s.key, s.rule, s.detail))
		}
	}
	for _, e := range entries {
		if err := checkClass(e.class); err != nil {
			problems = append(problems, fmt.Sprintf("sinks.tsv line %d (%s): %v",
				e.line, e.key, err))
		}
		if !seen[e.key] {
			problems = append(problems, fmt.Sprintf("stale row sinks.tsv line %d: %s "+
				"no longer writes; remove it", e.line, e.key))
		}
	}
	sort.Strings(problems)
	return problems
}
