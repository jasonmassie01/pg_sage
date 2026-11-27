package srebench

import (
	"fmt"
	"strconv"
	"strings"
)

// Environment that stamps a bench report with the pg_sage build it
// scores (roadmap 1.1): CI sets the tag (v1.8.5) and the commit, so the
// sidecar can refuse a report made for another build.
const (
	EnvPgSageVersion = "SAGE_BENCH_PG_SAGE_VERSION"
	EnvPgSageCommit  = "SAGE_BENCH_PG_SAGE_COMMIT"
	// EnvMinRunsPerFamily asks for at least this many scored top-1 runs
	// per family on every gated arm (benches-06): the initial repeats come
	// from RepeatsFor, and a top-up pass reruns the families that skips
	// left short. CI sets the promotion bar on tags. It conflicts with
	// EnvRepeats.
	EnvMinRunsPerFamily = "SAGE_BENCH_MIN_RUNS_PER_FAMILY"
)

// BuildFromEnv reads the pg_sage version (without a leading "v") and the
// lower-case commit; both are empty when unset.
func BuildFromEnv(getenv func(string) string) (version, commit string) {
	version = strings.TrimPrefix(strings.TrimSpace(getenv(EnvPgSageVersion)), "v")
	commit = strings.ToLower(strings.TrimSpace(getenv(EnvPgSageCommit)))
	return version, commit
}

// RepeatsFor is how many times every scenario must run so that each
// family among scenarios has at least minRuns top-1 (sufficient-evidence)
// runs: the smallest family decides, bounded by MaxRepeats. Families
// without a sufficient-evidence scenario never ask for repeats.
func RepeatsFor(scenarios []Scenario, minRuns int) int {
	sufficient := map[string]int{}
	for _, sc := range scenarios {
		if sc.Gold.Sufficient() {
			sufficient[string(sc.Family)]++
		}
	}
	repeats := 1
	for _, n := range sufficient {
		if need := (minRuns + n - 1) / n; need > repeats {
			repeats = need
		}
	}
	return min(repeats, MaxRepeats)
}

// ParseMinRuns reads EnvMinRunsPerFamily: empty means off (0).
func ParseMinRuns(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s=%q: want a positive integer (the scored top-1 runs "+
			"every family needs)", EnvMinRunsPerFamily, v)
	}
	return n, nil
}

// BenchRepeats reads the bench depth: EnvRepeats fixed repeats, or
// EnvMinRunsPerFamily, which sizes the repeats for scenarios with
// RepeatsFor and returns the bar for the top-up (0 when unset).
func BenchRepeats(getenv func(string) string, scenarios []Scenario) (repeats,
	minRuns int, err error) {
	minRuns, err = ParseMinRuns(getenv(EnvMinRunsPerFamily))
	if err != nil {
		return 0, 0, err
	}
	if minRuns == 0 {
		repeats, err = ParseRepeats(getenv(EnvRepeats))
		return repeats, 0, err
	}
	if v := strings.TrimSpace(getenv(EnvRepeats)); v != "" {
		return 0, 0, fmt.Errorf("%s=%q and %s=%d are both set: set one (the minimum "+
			"runs size the repeats themselves)", EnvRepeats, v, EnvMinRunsPerFamily, minRuns)
	}
	return RepeatsFor(scenarios, minRuns), minRuns, nil
}

// FamilyTop1Runs is, for every family with a sufficient-evidence scenario
// among scenarios, the fewest scored top-1 runs any of arms has in it.
// Skipped and errored runs are left out.
func FamilyTop1Runs(results []Result, scenarios []Scenario, arms []string) map[string]int {
	inArms := map[string]bool{}
	for _, a := range arms {
		inArms[a] = true
	}
	perArm := map[string]map[string]int{}
	for _, fam := range sufficientFamilies(scenarios) {
		perArm[fam] = map[string]int{}
		for _, a := range arms {
			perArm[fam][a] = 0
		}
	}
	for _, r := range results {
		fam := string(r.Scenario.Family)
		if counts, ok := perArm[fam]; ok && inArms[r.Arm] && scored(r) &&
			r.Scenario.Gold.Sufficient() {
			counts[r.Arm]++
		}
	}
	out := map[string]int{}
	for fam, counts := range perArm {
		if len(arms) == 0 {
			continue
		}
		least := -1
		for _, n := range counts {
			if least < 0 || n < least {
				least = n
			}
		}
		out[fam] = least
	}
	return out
}

// sufficientFamilies are the families of scenarios with a
// sufficient-evidence scenario, in first-seen order.
func sufficientFamilies(scenarios []Scenario) []string {
	seen := map[string]bool{}
	var out []string
	for _, sc := range scenarios {
		if fam := string(sc.Family); sc.Gold.Sufficient() && !seen[fam] {
			seen[fam] = true
			out = append(out, fam)
		}
	}
	return out
}
