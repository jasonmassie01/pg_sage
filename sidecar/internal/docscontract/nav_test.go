package docscontract

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Static file checks on mkdocs.yml and docs/. They run no code under test,
// share no state and have no lifecycle, so error propagation beyond the
// parser, concurrent access and state transitions do not apply here.

// releasedNavPages were in the nav before the docs site was cleaned up.
// Their URLs are public, so they must stay in the nav at the same paths.
var releasedNavPages = []string{
	"index.md", "quickstart.md", "walkthrough.md", "installation.md",
	"configuration.md", "architecture.md", "sql-reference.md", "findings.md",
	"facts.md", "mcp.md", "ask-sage.md", "specialist.md", "hint-verification.md",
	"security.md", "sage-sre-permissions-and-data-flow.md", "deployment.md",
	"azure.md", "databricks-native.md", "lakebase.md", "databricks-apps.md",
}

// userGuides are user pages that were published but unreachable.
var userGuides = []string{
	"agent-governance.md", "agent-safety-bench.md", "managed-clouds.md",
	"neon-supabase.md", "llm-costing.md", "try-it-out.md",
	"walkthrough-fleet.md", "walkthrough-linux.md", "walkthrough-windows.md",
}

func navSet(t *testing.T, cfg mkdocsConfig) map[string]bool {
	t.Helper()
	files, err := navFiles(cfg.Nav)
	if err != nil {
		t.Fatal(err)
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		if set[f] {
			t.Errorf("nav lists %s twice", f)
		}
		set[f] = true
	}
	return set
}

func TestEveryDocsPageIsInNavOrExcluded(t *testing.T) {
	cfg, docs := loadMkdocs(t)
	inNav := navSet(t, cfg)
	pages, err := docsPages(docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatalf("no Markdown pages under %s", docs)
	}
	var orphans []string
	for _, p := range pages {
		if !inNav[p] && !gitignoreMatch(cfg.NotInNav, p) {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) > 0 {
		t.Errorf("%d docs pages are neither in nav nor not_in_nav: %v",
			len(orphans), orphans)
	}
}

func TestNavEntriesExist(t *testing.T) {
	cfg, docs := loadMkdocs(t)
	files, err := navFiles(cfg.Nav)
	if err != nil {
		t.Fatal(err)
	}
	if missing := missingNavFiles(docs, files); len(missing) > 0 {
		t.Errorf("nav names files missing under %s: %v", docs, missing)
	}
}

func TestMissingNavFilesReportsMissingEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := missingNavFiles(dir, []string{"a.md", "gone.md", "sub", "sub/b.md"})
	want := []string{"gone.md", "sub", "sub/b.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingNavFiles = %v, want %v", got, want)
	}
	if got := missingNavFiles(dir, nil); got != nil {
		t.Errorf("missingNavFiles(nil) = %v, want nil", got)
	}
}

func TestNavParsedNonEmpty(t *testing.T) {
	cfg, _ := loadMkdocs(t)
	inNav := navSet(t, cfg)
	if len(inNav) < len(releasedNavPages)+len(userGuides) {
		t.Fatalf("nav has %d pages, want at least %d",
			len(inNav), len(releasedNavPages)+len(userGuides))
	}
	if !inNav["index.md"] {
		t.Error("nav lost the home page index.md")
	}
}

func TestParseMkdocsRejectsEmptyAndInvalid(t *testing.T) {
	cases := map[string]string{
		"no nav key":   "site_name: pg_sage\n",
		"empty nav":    "nav: []\n",
		"null nav":     "nav:\n",
		"empty file":   "",
		"broken yaml":  "nav: [index.md\n",
		"nav is a map": "nav: {Home: index.md}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMkdocs([]byte(body)); err == nil {
				t.Errorf("parseMkdocs(%q) accepted a config without a usable nav", body)
			}
		})
	}
}

func TestParseMkdocsDefaultsDocsDir(t *testing.T) {
	cfg, err := parseMkdocs([]byte("nav:\n  - index.md\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DocsDir != "docs" {
		t.Errorf("DocsDir = %q, want docs", cfg.DocsDir)
	}
}

func TestNavFilesFlattensSectionsAndSkipsURLs(t *testing.T) {
	cfg, err := parseMkdocs([]byte(`nav:
  - index.md
  - Guide: guide.md
  - Section:
      - Deep: a/deep.md
      - Deeper:
          - b/c.md
      - Repo: https://github.com/jasonmassie01/pg_sage
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := navFiles(cfg.Nav)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index.md", "guide.md", "a/deep.md", "b/c.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("navFiles = %v, want %v", got, want)
	}
	if _, err := navFiles([]any{42}); err == nil {
		t.Error("navFiles accepted a numeric nav entry")
	}
	if got, err := navFiles(nil); err != nil || got != nil {
		t.Errorf("navFiles(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestReleasedNavPagesKeepTheirPaths(t *testing.T) {
	cfg, _ := loadMkdocs(t)
	inNav := navSet(t, cfg)
	for _, p := range releasedNavPages {
		if !inNav[p] {
			t.Errorf("released page %s left the nav; its public URL would change", p)
		}
	}
}

func TestUserGuidesAreInNav(t *testing.T) {
	cfg, _ := loadMkdocs(t)
	inNav := navSet(t, cfg)
	for _, p := range userGuides {
		if !inNav[p] {
			t.Errorf("user guide %s is published but not reachable from the nav", p)
		}
	}
}

func TestSupersededFindingCatalogIsGone(t *testing.T) {
	_, docs := loadMkdocs(t)
	_, err := os.Stat(filepath.Join(docs, "FINDING_CATALOG.md"))
	if !os.IsNotExist(err) {
		t.Errorf("docs/FINDING_CATALOG.md still exists (stat err %v); findings.md "+
			"is the catalog", err)
	}
}

func TestNoInternalNotesPublished(t *testing.T) {
	cfg, docs := loadMkdocs(t)
	pages, err := docsPages(docs)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		if isInternalNote(p) {
			t.Errorf("internal note %s is under docs/ and would be published", p)
		}
	}
	if strings.TrimSpace(cfg.NotInNav) == "" {
		t.Error("mkdocs.yml has no not_in_nav; generated pages would warn as omitted")
	}
}

func TestIsInternalNoteBoundaries(t *testing.T) {
	internal := []string{
		"codex-bug-log.md", "codex-session-handoff-2026-04-27.md",
		"session-handoff.md", "plan_v0.8.5.md", "plan_v3.md",
		"superpowers/plans/x.md", "reverse_spec/01-architecture.md",
		"sub/superpowers/x.md",
	}
	public := []string{
		"index.md", "codex.md", "plan.md", "planning.md", "plan-v1.md",
		"walkthrough.md", "superpowers.md", "reverse_spec.md",
		"notsuperpowers/x.md", "",
	}
	for _, p := range internal {
		if !isInternalNote(p) {
			t.Errorf("isInternalNote(%q) = false, want true", p)
		}
	}
	for _, p := range public {
		if isInternalNote(p) {
			t.Errorf("isInternalNote(%q) = true, want false", p)
		}
	}
}

func TestGitignoreMatch(t *testing.T) {
	patterns := "# generated reference pages\n\ngenerated/*\nscratch/\n*.draft.md\n"
	cases := map[string]bool{
		"generated/config-lifecycles.md": true,
		"generated/derived-settings.md":  true,
		"generatedx.md":                  false,
		"sub/generated/x.md":             false,
		"scratch/a/b.md":                 true,
		"a/scratch/b.md":                 false,
		"x.draft.md":                     true,
		"deep/x.draft.md":                true,
		"index.md":                       false,
		"# generated reference pages":    false,
	}
	for rel, want := range cases {
		if got := gitignoreMatch(patterns, rel); got != want {
			t.Errorf("gitignoreMatch(%q) = %v, want %v", rel, got, want)
		}
	}
	if gitignoreMatch("", "index.md") {
		t.Error("empty pattern list matched a page")
	}
}

func TestStrictValidationConfigured(t *testing.T) {
	cfg, _ := loadMkdocs(t)
	want := map[string]map[string]string{
		"nav":   {"omitted_files": "warn", "not_found": "warn"},
		"links": {"not_found": "warn", "anchors": "warn"},
	}
	for section, keys := range want {
		for key, level := range keys {
			if got := cfg.Validation[section][key]; got != level {
				t.Errorf("validation.%s.%s = %q, want %q", section, key, got, level)
			}
		}
	}
}
