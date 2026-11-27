package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No concurrent access tests: the generator is a one-shot build tool with no
// shared state. No state-transition tests: an entry has no states.

const (
	committedDoc  = "../../../docs/glossary.md"
	committedJSON = "../../web/src/generated/glossary.json"
)

const sampleDoc = "# Glossary\n\nIntro prose is ignored.\n\n## The five nouns\n\n" +
	"### Change\n\nOne thing pg_sage proposes to do.\n\n" +
	"- Internal names: `recommendation`, `action`\n" +
	"- Avoid: Recommendation, proposal\n\n" +
	"## Other terms\n\n### Held back\n\nA Change the policy did not let run.\n"

func parseString(t *testing.T, doc string) ([]Entry, error) {
	t.Helper()
	return Parse(strings.NewReader(doc), "glossary.md")
}

func TestParse_HappyPath(t *testing.T) {
	entries, err := parseString(t, sampleDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}
	c := entries[0]
	if c.ID != "change" || c.Term != "Change" || c.Group != "The five nouns" {
		t.Errorf("first entry = %+v", c)
	}
	if c.Definition != "One thing pg_sage proposes to do." {
		t.Errorf("definition = %q", c.Definition)
	}
	if strings.Join(c.Internal, "|") != "recommendation|action" {
		t.Errorf("internal = %q, want backticks stripped", c.Internal)
	}
	if strings.Join(c.Avoid, "|") != "recommendation|proposal" {
		t.Errorf("avoid = %q, want lower-cased", c.Avoid)
	}
	h := entries[1]
	if h.ID != "held-back" || h.Group != "Other terms" || len(h.Avoid) != 0 {
		t.Errorf("second entry = %+v", h)
	}
	if h.Internal == nil || h.Avoid == nil {
		t.Error("absent lists must be empty slices so the JSON carries []")
	}
}

func TestParse_CRLFAndHTMLKeptVerbatim(t *testing.T) {
	doc := "### Issue\r\n\r\nSomething <b>wrong</b> & open.\r\n"
	entries, err := parseString(t, doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if entries[0].Definition != "Something <b>wrong</b> & open." {
		t.Errorf("definition = %q, want the HTML kept as text", entries[0].Definition)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"no definition", "## G\n\n### Issue\n\n### Plan\n\nB.\n",
			"glossary.md:3: term \"Issue\" has no definition"},
		{"no definition at end", "### Issue\n\n",
			"glossary.md:1: term \"Issue\" has no definition"},
		{"duplicate term", "### Issue\n\nA.\n\n### issue\n\nB.\n",
			"glossary.md:5: duplicate term \"issue\" (first at line 1)"},
		{"unknown list line", "### Issue\n\nA.\n- Synonyms: x\n",
			"glossary.md:4: malformed entry \"Issue\": unknown line"},
		{"two definition lines", "### Issue\n\nA.\nB.\n",
			"glossary.md:4: malformed entry \"Issue\": the definition must be one line"},
		{"list before definition", "### Issue\n\n- Avoid: case\n",
			"glossary.md:3: malformed entry \"Issue\": the definition must come first"},
		{"empty term", "### \n\nA.\n", "glossary.md:1: malformed entry: empty term"},
		{"empty list value", "### Issue\n\nA.\n- Avoid: case, ,x\n",
			"glossary.md:4: malformed entry \"Issue\": empty value in Avoid"},
		{"repeated list", "### Issue\n\nA.\n- Avoid: a\n- Avoid: b\n",
			"glossary.md:5: malformed entry \"Issue\": Avoid given twice"},
		{"avoid a term", "### Issue\n\nA.\n- Avoid: change\n\n### Change\n\nB.\n",
			"glossary.md:4: avoid word \"change\" of \"Issue\" is itself a glossary term"},
		{"avoid twice", "### Issue\n\nA.\n- Avoid: x\n\n### Plan\n\nB.\n- Avoid: x\n",
			"glossary.md:9: avoid word \"x\" of \"Plan\" is already avoided by \"Issue\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := parseString(t, tc.doc)
			if err == nil {
				t.Fatalf("Parse succeeded with %+v, want %q", entries, tc.want)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("err = %q, want prefix %q", err, tc.want)
			}
		})
	}
}

func TestParse_DefinitionLengthBoundary(t *testing.T) {
	def := strings.Repeat("é", maxDefinition)
	entries, err := parseString(t, "### Issue\n\n"+def+"\n")
	if err != nil {
		t.Fatalf("a %d-character definition must pass: %v", maxDefinition, err)
	}
	if entries[0].Definition != def {
		t.Error("the 280-character definition was not kept whole")
	}
	_, err = parseString(t, "### Issue\n\n"+def+"x\n")
	want := "glossary.md:3: definition of \"Issue\" is 281 characters; the limit is 280"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestParse_EmptyDoc(t *testing.T) {
	entries, err := parseString(t, "")
	if err != nil || len(entries) != 0 {
		t.Fatalf("Parse(\"\") = %v, %v; want no entries and no error", entries, err)
	}
	out, err := Render(entries)
	if err != nil || string(out) != "{}\n" {
		t.Errorf("Render(empty) = %q, %v; want \"{}\\n\"", out, err)
	}
}

func TestRender_KeyedByID(t *testing.T) {
	entries, err := parseString(t, sampleDoc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(entries)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]struct {
		Term, Group, Definition string
		Internal, Avoid         []string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Render wrote invalid JSON: %v\n%s", err, out)
	}
	if got["change"].Term != "Change" || got["held-back"].Definition == "" {
		t.Errorf("rendered = %+v", got)
	}
	if !strings.Contains(string(out), "\"avoid\": []") {
		t.Errorf("an entry with no avoid list must render []:\n%s", out)
	}
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_WritesAndChecks(t *testing.T) {
	doc := writeTemp(t, "glossary.md", sampleDoc)
	out := filepath.Join(t.TempDir(), "gen", "glossary.json")
	if err := run([]string{"-doc", doc, "-out", out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(raw), "\"held-back\"") {
		t.Fatalf("output = %q, %v", raw, err)
	}
	if err := run([]string{"-doc", doc, "-out", out, "-check"}); err != nil {
		t.Errorf("-check on a fresh file: %v", err)
	}
	if err := os.WriteFile(out, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-doc", doc, "-out", out, "-check"})
	if err == nil || !strings.Contains(err.Error(), "is stale") {
		t.Errorf("-check on a stale file: err = %v, want \"is stale\"", err)
	}
}

func TestRun_Errors(t *testing.T) {
	out := filepath.Join(t.TempDir(), "glossary.json")
	err := run([]string{"-doc", filepath.Join(t.TempDir(), "missing.md"), "-out", out})
	if err == nil || !strings.Contains(err.Error(), "read glossary") {
		t.Errorf("missing doc: err = %v, want \"read glossary\"", err)
	}
	bad := writeTemp(t, "glossary.md", "### Issue\n")
	err = run([]string{"-doc", bad, "-out", out})
	if err == nil || !strings.Contains(err.Error(), "glossary.md:1:") {
		t.Errorf("bad doc: err = %v, want the line number", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("a failed parse must write nothing")
	}
	if err := run([]string{"-nope"}); err == nil {
		t.Error("an unknown flag must fail")
	}
}

// Golden: the committed glossary.json is exactly what the doc generates
// (line endings aside: checkouts may use CRLF).
func TestCommittedGlossaryMatchesDoc(t *testing.T) {
	if err := run([]string{"-doc", committedDoc, "-out", committedJSON, "-check"}); err != nil {
		t.Fatalf("%v: regenerate with `go run ./cmd/gen_glossary/` from sidecar/", err)
	}
	entries, err := readDoc(committedDoc)
	if err != nil {
		t.Fatal(err)
	}
	nouns := []string{}
	for _, e := range entries {
		if e.Group == "The five nouns" {
			nouns = append(nouns, e.Term)
		}
	}
	if strings.Join(nouns, ",") != "Issue,Plan,Change,Decision,Autonomy" {
		t.Errorf("the five nouns = %v", nouns)
	}
	for _, jargon := range r5Jargon {
		if !covers(entries, jargon) {
			t.Errorf("R5 §3.1 term %q has no definition or replacement", jargon)
		}
	}
}

// r5Jargon is the R5 §3.1 list of words shown without a definition.
var r5Jargon = []string{
	"standing gate", "policy gate", "verdict", "parked", "observe-only",
	"shadow decision", "contract tier", "lifecycle state", "custodian", "canary",
	"grandfathered", "promotion floor", "fast elevation", "trust ramp", "tier3_safe",
	"hypopg", "managed change", "binding fact", "source fix", "specialist contract",
}

// covers reports whether word is an entry's term, internal name or avoid word.
func covers(entries []Entry, word string) bool {
	for _, e := range entries {
		names := append(append([]string{e.Term}, e.Internal...), e.Avoid...)
		for _, n := range names {
			if strings.EqualFold(n, word) {
				return true
			}
		}
	}
	return false
}
