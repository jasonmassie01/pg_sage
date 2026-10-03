package srebench

import "strings"

// Environment that stamps a bench report with the pg_sage build it
// scores (roadmap 1.1): CI sets the tag (v1.8.5) and the commit, so the
// sidecar can refuse a report made for another build.
const (
	EnvPgSageVersion = "SAGE_BENCH_PG_SAGE_VERSION"
	EnvPgSageCommit  = "SAGE_BENCH_PG_SAGE_COMMIT"
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
