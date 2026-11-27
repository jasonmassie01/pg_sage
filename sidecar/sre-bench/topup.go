package srebench

import (
	"fmt"
	"sort"
)

// topUp lists what a release run reruns to reach minRuns scored top-1
// runs per family on every one of arms (benches-06): the scored
// sufficient-evidence scenarios of each family short of the bar, in
// scenario order. A short family none of whose sufficient-evidence
// scenarios scored on any arm cannot be topped up: it is returned in
// unreachable instead, so the caller does not loop on it.
func topUp(results []Result, scenarios []Scenario, arms []string,
	minRuns int) (rerun []Scenario, unreachable []string) {
	if minRuns <= 0 || len(arms) == 0 {
		return nil, nil
	}
	inArms := map[string]bool{}
	for _, a := range arms {
		inArms[a] = true
	}
	scoredIDs := map[string]bool{}
	for _, r := range results {
		if inArms[r.Arm] && scored(r) {
			scoredIDs[r.Scenario.ID] = true
		}
	}
	counts := FamilyTop1Runs(results, scenarios, arms)
	for _, fam := range sufficientFamilies(scenarios) {
		if counts[fam] >= minRuns {
			continue
		}
		before := len(rerun)
		for _, sc := range scenarios {
			if string(sc.Family) == fam && sc.Gold.Sufficient() && scoredIDs[sc.ID] {
				rerun = append(rerun, sc)
			}
		}
		if len(rerun) == before {
			unreachable = append(unreachable, fam)
		}
	}
	return rerun, unreachable
}

// topUpRuns reruns topUp's scenarios one repeat at a time, numbering the
// repeats on from done, until every family reaches minRuns or the run
// reaches MaxRepeats. run runs scenarios as the given repeat.
func topUpRuns(results []Result, scenarios []Scenario, arms []string, minRuns,
	done int, run func(ss []Scenario, repeat int) []Result) []Result {
	for rep := done + 1; rep <= MaxRepeats; rep++ {
		rerun, _ := topUp(results, scenarios, arms, minRuns)
		if len(rerun) == 0 {
			break
		}
		results = append(results, run(rerun, rep)...)
	}
	return results
}

// ShortFamilies describes every family with fewer than minRuns scored
// top-1 runs on one of arms, sorted; nothing when minRuns is 0.
func ShortFamilies(results []Result, scenarios []Scenario, arms []string,
	minRuns int) []string {
	if minRuns <= 0 {
		return nil
	}
	_, unreachable := topUp(results, scenarios, arms, minRuns)
	none := map[string]bool{}
	for _, fam := range unreachable {
		none[fam] = true
	}
	var out []string
	for fam, n := range FamilyTop1Runs(results, scenarios, arms) {
		if n >= minRuns {
			continue
		}
		why := fmt.Sprintf("after %d repeats (the bench maximum)", MaxRepeats)
		if none[fam] {
			why = "unreachable: every sufficient-evidence scenario was skipped or failed"
		}
		out = append(out, fmt.Sprintf("family %s: n=%d against bar %d, %s", fam, n,
			minRuns, why))
	}
	sort.Strings(out)
	return out
}
