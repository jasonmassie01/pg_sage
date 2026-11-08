package srebench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// EnvExpected names the expected-scenario manifest (benches-06). CI sets
// it on tags; a run with it fails when a scenario the manifest expects on
// the server's PG major is skipped, or when skips cost a gated arm a gate
// it would have evaluated. Unset, skips are reported and do not fail.
const EnvExpected = "SAGE_BENCH_EXPECTED"

// ExpectedManifest is, per PG major, the scenarios a release run must
// score and the skips it allows (scenario id to the reason, such as a
// fixture CI's image does not have). Every scenario is in exactly one of
// the two lists, so a new scenario cannot be left out silently.
type ExpectedManifest struct {
	Description string                      `json:"description,omitempty"`
	Majors      map[string]MajorExpectation `json:"majors"`
}

// MajorExpectation is one PG major's entry of the manifest.
type MajorExpectation struct {
	Expected     []string          `json:"expected"`
	AllowedSkips map[string]string `json:"allowed_skips,omitempty"`
}

// LoadExpected reads and validates the manifest at path against all.
func LoadExpected(path string, all []Scenario) (ExpectedManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ExpectedManifest{}, fmt.Errorf("%s: read manifest %s: %w", EnvExpected, path,
			err)
	}
	m, err := ParseExpected(raw, all)
	if err != nil {
		return ExpectedManifest{}, fmt.Errorf("%s: manifest %s: %w", EnvExpected, path, err)
	}
	return m, nil
}

// ParseExpected decodes a manifest and checks it against all: no unknown
// ids, none twice, every scenario expected or allowed to skip with a
// reason, and no major without expected scenarios.
func ParseExpected(raw []byte, all []Scenario) (ExpectedManifest, error) {
	var m ExpectedManifest
	if len(bytes.TrimSpace(raw)) == 0 {
		return m, fmt.Errorf("manifest lists no scenarios (the file is empty)")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("parse manifest: %w", err)
	}
	if len(m.Majors) == 0 {
		return m, fmt.Errorf("manifest lists no scenarios (no PG majors)")
	}
	majors := make([]string, 0, len(m.Majors))
	for k := range m.Majors {
		majors = append(majors, k)
	}
	sort.Strings(majors)
	for _, k := range majors {
		if err := checkMajor(k, m.Majors[k], all); err != nil {
			return m, err
		}
	}
	return m, nil
}

func checkMajor(key string, e MajorExpectation, all []Scenario) error {
	if n, err := strconv.Atoi(key); err != nil || n < 1 {
		return fmt.Errorf("major %q: want a PostgreSQL major version such as \"17\"", key)
	}
	if len(e.Expected) == 0 {
		return fmt.Errorf("PG %s: manifest lists no scenarios", key)
	}
	known := map[string]bool{}
	for _, sc := range all {
		known[sc.ID] = true
	}
	listed := map[string]bool{}
	add := func(id string) error {
		switch {
		case !known[id]:
			return fmt.Errorf("PG %s: unknown scenario %q", key, id)
		case listed[id]:
			return fmt.Errorf("PG %s: %q is listed twice", key, id)
		}
		listed[id] = true
		return nil
	}
	for _, id := range e.Expected {
		if err := add(id); err != nil {
			return err
		}
	}
	for _, id := range sortedKeys(e.AllowedSkips) {
		if err := add(id); err != nil {
			return err
		}
		if strings.TrimSpace(e.AllowedSkips[id]) == "" {
			return fmt.Errorf("PG %s: allowed skip %q has no reason", key, id)
		}
	}
	for _, sc := range all {
		if !listed[sc.ID] {
			return fmt.Errorf("PG %s: scenario %q is neither expected nor an allowed skip",
				key, sc.ID)
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// For is the set of scenarios the manifest expects scored on major.
func (m ExpectedManifest) For(major int) (map[string]bool, error) {
	e, ok := m.Majors[strconv.Itoa(major)]
	if !ok {
		return nil, fmt.Errorf("%s: the manifest has no entry for PostgreSQL %d: add one",
			EnvExpected, major)
	}
	out := make(map[string]bool, len(e.Expected))
	for _, id := range e.Expected {
		out[id] = true
	}
	return out, nil
}

// ExpectedSkips describes every expected scenario that was skipped in
// any run, with how many runs and the first Unsupported reason, sorted.
func ExpectedSkips(expected map[string]bool, results []Result) []string {
	count, reason := map[string]int{}, map[string]string{}
	for _, r := range results {
		if r.Skipped == "" || !expected[r.Scenario.ID] {
			continue
		}
		if count[r.Scenario.ID] == 0 {
			reason[r.Scenario.ID] = r.Skipped
		}
		count[r.Scenario.ID]++
	}
	out := make([]string, 0, len(count))
	for id, n := range count {
		out = append(out, fmt.Sprintf("expected scenario %s was skipped in %d run(s): %s",
			id, n, reason[id]))
	}
	sort.Strings(out)
	return out
}

// familyGates are the §12 gates evaluated per family from the scored
// runs; the others (pooled, model parity and root, replay) do not depend
// on which fault programs were skipped.
var familyGates = map[string]bool{GateTop1: true, GateAbstention: true, GateForbidden: true,
	GateNoise: true, GateDecoy: true, GatePacket: true}

// LostGates are the not-evaluated per-family gates of arms that skips
// cost: the runs the arm attempted in the family would have fed the
// gate, its scored runs do not. A gate the scenarios cannot feed (a
// family without noise variants) or one a fake model never evaluates
// is structural and not returned.
func LostGates(gates []GateResult, results []Result, arms []string) []GateResult {
	inArms := map[string]bool{}
	for _, a := range arms {
		inArms[a] = true
	}
	var out []GateResult
	for _, g := range gates {
		if g.Status != GateNotEvaluated || !inArms[g.Arm] || g.Family == PooledFamily ||
			!familyGates[g.ID] || g.Reason == fakeModelReason {
			continue
		}
		var ran, kept []Scenario
		for _, r := range results {
			if r.Arm != g.Arm || string(r.Scenario.Family) != g.Family {
				continue
			}
			ran = append(ran, r.Scenario)
			if scored(r) {
				kept = append(kept, r.Scenario)
			}
		}
		if feeds(g.ID, ran) && !feeds(g.ID, kept) {
			out = append(out, g)
		}
	}
	return out
}

// feeds reports whether runs of ss give gate id its data.
func feeds(id string, ss []Scenario) bool {
	has := func(ok func(Scenario) bool) bool {
		for _, sc := range ss {
			if ok(sc) {
				return true
			}
		}
		return false
	}
	class := func(c string) func(Scenario) bool {
		return func(sc Scenario) bool { return sc.Class == c }
	}
	switch id {
	case GateTop1:
		return has(func(sc Scenario) bool { return sc.Gold.Sufficient() })
	case GateAbstention:
		return has(func(sc Scenario) bool { return !sc.Gold.Sufficient() })
	case GateNoise:
		return has(class(ClassNoise)) && has(class(ClassPositive))
	case GateDecoy:
		return has(class(ClassDecoy)) && has(class(ClassPositive))
	default:
		return len(ss) > 0
	}
}
