package replay

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// Roadmap 2.4: the replay corpus is split deterministically into a
// tuning set and a held-out set by a stable hash of each case id. Gates
// and the model-root override rule read the held-out set only. The split
// of every case is pinned in split.lock, so a renamed or added case shows
// its split in review and a case can never move silently between sets.

// No concurrent access tests: the split is a pure function of the id.

func TestSplitOf_PinnedBuckets(t *testing.T) {
	for id, bucket := range map[string]int{
		"lock-idle-holder-row-waits": 46, "wal-slow-consumer": 55, "conn-leak-burst": 29,
		"plan-flip-no-slowdown": 84, "wal-archiver-failing": 90, "conn-pool-warmup": 60,
		"lock-deadlock-cycle": 37,
	} {
		if got := SplitBucket(id); got != bucket {
			t.Errorf("SplitBucket(%q) = %d, want %d (the split hash changed)", id, got, bucket)
		}
		want := SplitTuning
		if bucket < HeldOutPercent {
			want = SplitHeldOut
		}
		if got := SplitOf(id); got != want {
			t.Errorf("SplitOf(%q) = %s, want %s", id, got, want)
		}
	}
}

func TestSplitForBucket_Boundaries(t *testing.T) {
	if HeldOutPercent != 50 {
		t.Fatalf("HeldOutPercent = %d; changing it moves cases between sets", HeldOutPercent)
	}
	for b, want := range map[int]string{0: SplitHeldOut, 49: SplitHeldOut, 50: SplitTuning,
		99: SplitTuning} {
		if got := splitForBucket(b); got != want {
			t.Errorf("bucket %d = %s, want %s", b, got, want)
		}
	}
}

func TestSplitOf_RoughlyBalanced(t *testing.T) {
	held := 0
	const n = 2000
	for i := 0; i < n; i++ {
		if SplitOf(fmt.Sprintf("synthetic-case-%04d", i)) == SplitHeldOut {
			held++
		}
	}
	if held < n*45/100 || held > n*55/100 {
		t.Fatalf("%d of %d synthetic ids held out, want about half", held, n)
	}
	if SplitOf("") != SplitOf("") || SplitBucket("x") < 0 || SplitBucket("x") > 99 {
		t.Fatal("the split must be deterministic and bucketed 0-99")
	}
}

// readLock reads split.lock: "<id> <split>" per line, # comments.
func readLock(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("split.lock")
	if err != nil {
		t.Fatalf("split.lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSplitLock_PinsEveryCorpusCase(t *testing.T) {
	cases := loadCorpus(t)
	want := make([]string, 0, len(cases))
	for _, c := range cases {
		want = append(want, c.ID+" "+SplitOf(c.ID))
	}
	sort.Strings(want)
	got := readLock(t)
	if !sort.StringsAreSorted(got) {
		t.Error("split.lock must be sorted by case id")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("split.lock is stale. Never rename a case to change its split; to add "+
			"a case, add its line. Expected contents:\n%s", strings.Join(want, "\n"))
	}
}

func TestSplit_EveryFamilyHasHeldOutEvidenceBothWays(t *testing.T) {
	type key struct{ family, kind string }
	seen := map[key]int{}
	for _, c := range loadCorpus(t) {
		if SplitOf(c.ID) != SplitHeldOut {
			continue
		}
		kind := "insufficient"
		if c.Sufficient() {
			kind = "sufficient"
		}
		seen[key{c.Family, kind}]++
	}
	for _, fam := range append(append([]string(nil), r1Families...), "plan_regression") {
		for _, kind := range []string{"sufficient", "insufficient"} {
			if seen[key{fam, kind}] == 0 {
				t.Errorf("%s has no held-out %s case: its held-out gates cannot be "+
					"evaluated", fam, kind)
			}
		}
	}
}

func TestFilterSplit(t *testing.T) {
	cases := loadCorpus(t)
	all, err := FilterSplit(cases, "")
	if err != nil || len(all) != len(cases) {
		t.Fatalf("empty split = %d (%v)", len(all), err)
	}
	if again, err := FilterSplit(cases, SplitAll); err != nil || len(again) != len(cases) {
		t.Fatalf("all = %d (%v)", len(again), err)
	}
	held, errH := FilterSplit(cases, SplitHeldOut)
	tuning, errT := FilterSplit(cases, SplitTuning)
	if errH != nil || errT != nil || len(held)+len(tuning) != len(cases) || len(held) == 0 ||
		len(tuning) == 0 {
		t.Fatalf("held %d + tuning %d != %d (%v %v)", len(held), len(tuning), len(cases),
			errH, errT)
	}
	for _, c := range held {
		if SplitOf(c.ID) != SplitHeldOut {
			t.Fatalf("%s is not held out", c.ID)
		}
	}
	if _, err := FilterSplit(cases, "test"); err == nil {
		t.Fatal("an unknown split must be refused")
	}
	if got, err := FilterSplit(nil, SplitHeldOut); err != nil || len(got) != 0 {
		t.Fatalf("nil cases = %v (%v)", got, err)
	}
}
