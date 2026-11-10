package docscontract

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// publishedPages reads every page MkDocs will publish, keyed by its
// docs-relative path.
func publishedPages(t *testing.T, docs string) map[string]string {
	t.Helper()
	pages, err := docsPages(docs)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(pages))
	for _, p := range pages {
		body, err := os.ReadFile(filepath.Join(docs, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("read docs/%s: %v", p, err)
		}
		out[p] = string(body)
	}
	return out
}

func TestRelativeLinksResolveInsideDocs(t *testing.T) {
	_, docs := loadMkdocs(t)
	published := publishedPages(t, docs)
	names := make([]string, 0, len(published))
	for p := range published {
		names = append(names, p)
	}
	sort.Strings(names)
	checked := 0
	for _, p := range names {
		for _, target := range linkTargets(published[p]) {
			checked++
			if problem := linkProblem(docs, p, target, published); problem != "" {
				t.Error(problem)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no links in docs/; the link scanner is broken")
	}
}

func TestLinkTargetsSkipsCode(t *testing.T) {
	body := "See [setup](installation.md#database-user-setup) and " +
		"![logo](img/logo.png \"Logo\").\n" +
		"Inline `[not](a-link.md)` code.\n" +
		"```markdown\n[fenced](fenced.md)\n```\n" +
		"  ~~~\n[tilde](tilde.md)\n  ~~~\n" +
		"[ref]: <reference.md>\n" +
		"[two](a.md) and [three](https://example.com/x)\n"
	got := linkTargets(body)
	want := []string{
		"installation.md#database-user-setup", "img/logo.png", "reference.md",
		"a.md", "https://example.com/x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linkTargets = %q, want %q", got, want)
	}
	if got := linkTargets(""); got != nil {
		t.Errorf("linkTargets(empty) = %q, want nil", got)
	}
}

func TestSlugifyMatchesToc(t *testing.T) {
	cases := map[string]string{
		"Secrets at rest":                          "secrets-at-rest",
		"1. Create a role for pg_sage":             "1-create-a-role-for-pg_sage",
		"Advanced telemetry: auto_explain and SRE": "advanced-telemetry-auto_explain-and-sre",
		"Install & run — fast":                     "install-run-fast",
		"  Trailing   spaces  ":                    "trailing-spaces",
		"CSRF and cross-origin requests":           "csrf-and-cross-origin-requests",
		"":                                         "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPageAnchors(t *testing.T) {
	body := "# Title\n## `sage.findings` table\n## Usage\n### Usage\n" +
		"## Custom { #my-id }\n## [Linked](x.md) **bold**\n" +
		"<a id=\"raw-anchor\"></a>\n```bash\n# not a heading\n```\n"
	got := pageAnchors(body)
	want := map[string]bool{
		"title": true, "sagefindings-table": true, "usage": true, "usage_1": true,
		"my-id": true, "linked-bold": true, "raw-anchor": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pageAnchors = %v, want %v", got, want)
	}
	if got := pageAnchors(""); len(got) != 0 {
		t.Errorf("pageAnchors(empty) = %v, want none", got)
	}
}

func TestLinkProblem(t *testing.T) {
	docs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(docs, "img"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "img", "a.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	published := map[string]string{
		"index.md":     "# Home\n## Part one\n",
		"guide/doc.md": "# Doc\n",
	}
	ok := []string{
		"index.md", "index.md#part-one", "#part-one", "guide/doc.md",
		"img/a.png", "https://github.com/x", "mailto:a@b.c", "/abs/path",
	}
	for _, target := range ok {
		if p := linkProblem(docs, "index.md", target, published); p != "" {
			t.Errorf("linkProblem(%q) = %q, want none", target, p)
		}
	}
	if p := linkProblem(docs, "guide/doc.md", "../index.md#home", published); p != "" {
		t.Errorf("parent link inside docs: %q", p)
	}
	bad := []string{
		"../README.md", "../sidecar/internal/", "missing.md", "index.md#nope",
		"#nope", "img/missing.png", "img", "guide/../../x.md",
	}
	for _, target := range bad {
		if p := linkProblem(docs, "index.md", target, published); p == "" {
			t.Errorf("linkProblem(%q) accepted a link MkDocs cannot resolve", target)
		}
	}
}
