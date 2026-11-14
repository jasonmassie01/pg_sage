package srebench

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// No concurrency, invalid-input or error-propagation cases: the catalog is
// built by pure constructors that take no input and share no state.

var updateScenarioGolden = flag.Bool("update-scenarios", false,
	"rewrite testdata/scenarios.golden from the current catalog")

const scenarioGoldenPath = "testdata/scenarios.golden"

// programSteps names the steps a program defines, in run order, so a moved
// or dropped step changes the golden.
func programSteps(p Program) string {
	prog, ok := p.(program)
	if !ok {
		return fmt.Sprintf("type=%T", p)
	}
	var steps []string
	for _, s := range []struct {
		name string
		set  bool
	}{
		{"inject", prog.inject != nil}, {"manifest", prog.manifest != nil},
		{"between", prog.between != nil}, {"valid", prog.valid != nil},
		{"recover", prog.recover != nil},
	} {
		if s.set {
			steps = append(steps, s.name)
		}
	}
	return fmt.Sprintf("steps=%d[%s]", len(steps), strings.Join(steps, ","))
}

// uniqueSuffix is the per-process part of a uniqueName: a clock value and
// a process-wide counter.
var uniqueSuffix = regexp.MustCompile(`_[0-9]{15,}_[0-9]+`)

// stableSubject removes what differs between processes: uniqueName
// suffixes, and the run's planBase from a plan scenario's query id.
func stableSubject(subject string) string {
	subject = uniqueSuffix.ReplaceAllString(subject, "_<unique>")
	if rest, ok := strings.CutPrefix(subject, "queryid "); ok {
		if qid, err := strconv.ParseInt(rest, 10, 64); err == nil {
			return fmt.Sprintf("queryid planBase+%d", qid-planBase)
		}
	}
	return subject
}

// scenarioCatalog renders every registered scenario as one line: what a
// signed bench result means depends on exactly this list.
func scenarioCatalog(scenarios []Scenario) string {
	var b strings.Builder
	for _, s := range scenarios {
		fmt.Fprintf(&b, "%s family=%s class=%s subject=%q split=%q "+
			"root=%q contributing=%q lookalike=%q %s\n",
			s.ID, s.Family, s.Class, stableSubject(s.Subject), s.Split, s.Gold.Root,
			s.Gold.Contributing, s.Gold.Lookalike, programSteps(s.Program))
	}
	return b.String()
}

func TestScenarioCatalog_MatchesGolden(t *testing.T) {
	got := scenarioCatalog(Scenarios())
	if *updateScenarioGolden {
		if err := os.WriteFile(filepath.FromSlash(scenarioGoldenPath),
			[]byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", scenarioGoldenPath, err)
		}
	}
	raw, err := os.ReadFile(filepath.FromSlash(scenarioGoldenPath))
	if err != nil {
		t.Fatalf("read %s (run with -update-scenarios on unchanged code): %v",
			scenarioGoldenPath, err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got != want {
		t.Fatalf("scenario catalog changed; a signed bench result would mean "+
			"something else.\n--- got\n%s--- want\n%s", got, want)
	}
}

// The WAL family is the code this split moves; pin its order, family and
// steps directly so a failure names the moved scenario.
func TestScenarioCatalog_WALFamily(t *testing.T) {
	const all = "steps=5[inject,manifest,between,valid,recover]"
	want := []struct{ id, steps string }{
		{"wal-inactive-slot", all},
		{"wal-slow-consumer", all},
		{"wal-write-surge", all},
		{"wal-archiver-failure", "steps=4[inject,manifest,between,recover]"},
		{"wal-inactive-slot-under-load", all},
		{"wal-write-surge-under-load", all},
		{"wal-slot-keeping-up", all},
		{"wal-steady", all},
	}
	got := walScenarios()
	if len(got) != len(want) {
		t.Fatalf("walScenarios() = %d scenarios, want %d", len(got), len(want))
	}
	for i, w := range want {
		s := got[i]
		if s.ID != w.id {
			t.Errorf("walScenarios()[%d].ID = %q, want %q", i, s.ID, w.id)
		}
		if s.Family != sre.TriggerWAL {
			t.Errorf("%s: family = %q, want %q", s.ID, s.Family, sre.TriggerWAL)
		}
		if steps := programSteps(s.Program); steps != w.steps {
			t.Errorf("%s: %s, want %s", s.ID, steps, w.steps)
		}
	}
}

func TestStableSubject(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"table public.bench_wrap_1794674163585670700_30",
			"table public.bench_wrap_<unique>"},
		{"sequence public.bench_seq_1794674163585670700_49_seq",
			"sequence public.bench_seq_<unique>_seq"},
		{fmt.Sprintf("queryid %d", planBase+20), "queryid planBase+20"},
		{"queryid notanumber", "queryid notanumber"},
		{"table public.bench_wrap_12_30", "table public.bench_wrap_12_30"},
	} {
		if got := stableSubject(c.in); got != c.want {
			t.Errorf("stableSubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScenarioCatalog_EmptyIsEmpty(t *testing.T) {
	if got := scenarioCatalog(nil); got != "" {
		t.Fatalf("scenarioCatalog(nil) = %q, want empty", got)
	}
}
