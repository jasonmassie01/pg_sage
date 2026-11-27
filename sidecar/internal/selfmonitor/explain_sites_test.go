package selfmonitor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// No concurrent access tests: the check is a pure function of source files.

func fixtureExplainTree(t *testing.T) explainTree {
	t.Helper()
	tree, err := scanExplainTree(filepath.Join("testdata", "explainsites"), "internal")
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

const fixtureFile = "internal/probe/probe.go"

func TestExplainSites_FindsEveryLiteralOpeningWithExplain(t *testing.T) {
	var got []string
	for _, s := range fixtureExplainTree(t).sites {
		got = append(got, s.name)
	}
	want := []string{"pinned", "unpinned", "session.plan", "ownPlan", "keywords"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sites = %v, want %v (prose and EXPLAINED are not sites)", got, want)
	}
}

func TestExplainSites_FullRatchetPasses(t *testing.T) {
	entries, problems := parseSiteEntries(strings.Join([]string{
		"# fixture",
		fixtureFile + " pinned pin",
		fixtureFile + " unpinned via session.open -- the caller opens the session",
		fixtureFile + " session.plan via session.open -- planned in the opened session",
		fixtureFile + " ownPlan own ownPath -- the grant's path",
		fixtureFile + " keywords text -- a keyword list",
	}, "\n"))
	if len(problems) != 0 {
		t.Fatalf("parse problems: %v", problems)
	}
	if got := checkExplainSites(fixtureExplainTree(t), entries); len(got) != 0 {
		t.Fatalf("problems = %v, want none", got)
	}
}

func TestExplainSites_ReportsUnpinnedMissingAndStale(t *testing.T) {
	entries, _ := parseSiteEntries(strings.Join([]string{
		fixtureFile + " pinned pin",
		fixtureFile + " unpinned pin",
		fixtureFile + " session.plan via unpinned -- wrong opener",
		fixtureFile + " ownPlan own pinned -- pinned sets no path",
		fixtureFile + " gone pin",
	}, "\n"))
	got := strings.Join(checkExplainSites(fixtureExplainTree(t), entries), "\n")
	for _, want := range []string{
		"unpinned: does not call apppath.Pin",
		"session.plan: unpinned does not call apppath.Pin",
		"ownPlan: pinned sets no search_path",
		"keywords holds an EXPLAIN and is not in explain_sites.txt",
		"line 5: " + fixtureFile + " gone holds no EXPLAIN (stale entry)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, " pinned: ") {
		t.Errorf("the pinned function was reported:\n%s", got)
	}
}

func TestExplainSites_EntryShapeNeedsAReason(t *testing.T) {
	for _, line := range []string{
		"a.go f text",
		"a.go f via g",
		"a.go f own",
		"a.go f pin extra",
		"a.go f nonsense -- why",
		"a.go f",
	} {
		entries, problems := parseSiteEntries(line)
		if len(entries) != 0 || len(problems) != 1 {
			t.Errorf("%q: entries %v problems %v, want one problem", line, entries, problems)
		}
	}
	entries, problems := parseSiteEntries("")
	if len(entries) != 0 || len(problems) != 0 {
		t.Fatalf("empty ratchet: entries %v problems %v", entries, problems)
	}
}

func readExplainRatchet(t *testing.T) []siteEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "explain_sites.txt"))
	if err != nil {
		t.Fatal(err)
	}
	entries, problems := parseSiteEntries(string(raw))
	if len(problems) != 0 {
		t.Fatalf("explain_sites.txt: %v", problems)
	}
	return entries
}

// Every production function that holds an EXPLAIN is listed, and every
// one that plans an application's statement sets the application's path.
func TestExplainSites_ProductionMatchesTheRatchet(t *testing.T) {
	tree, err := scanExplainTree(filepath.Join("..", ".."), "internal", "cmd")
	if err != nil {
		t.Fatal(err)
	}
	if problems := checkExplainSites(tree, readExplainRatchet(t)); len(problems) != 0 {
		t.Fatalf("explain sites (security-01c-n):\n%s", strings.Join(problems, "\n"))
	}
}

// The plan's audit named these files (CHECK-07); each is in the ratchet
// with the mode its code has.
func TestExplainSites_RatchetCoversTheAuditedFiles(t *testing.T) {
	modes := map[string]map[string]bool{}
	for _, e := range readExplainRatchet(t) {
		if modes[e.file] == nil {
			modes[e.file] = map[string]bool{}
		}
		modes[e.file][e.mode] = true
	}
	for file, want := range map[string]string{
		"internal/agentguard/broker/explain.go": "own",
		"internal/autoexplain/collector.go":     "pin",
		"internal/explain/explain.go":           "via",
		"internal/optimizer/hypopg_session.go":  "via",
		"internal/optimizer/hypopg_generic.go":  "via",
		"internal/sre/investigator_explain.go":  "pin",
		"internal/tuner/hint_explain.go":        "pin",
		"internal/tuning/store_plan.go":         "via",
		"internal/tuning/rehearsal_measure.go":  "via",
		"internal/verify/ladder_session.go":     "via",
		"internal/briefing/ground.go":           "text",
		"internal/cases/query_actions.go":       "text",
	} {
		if !modes[file][want] {
			t.Errorf("%s: modes %v, want one %q entry", file, modes[file], want)
		}
	}
}
