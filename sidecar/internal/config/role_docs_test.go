package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// firstlook-02: every page that creates pg_sage's role must create the role
// pg_sage can bootstrap with. The try-it and walkthrough pages start a stock
// postgres image, where the role has no CREATE on the database, so without
// CREATE SCHEMA ... sage the bootstrap fails and the database is refused.
//
// No concurrency or state-transition tests: these read static files only.

const docsGlob = "../../../docs/*.md"

// Pages that start their own postgres image and must reuse the quickstart's
// canonical, e2e-tested role block verbatim.
var tryItPages = []string{"try-it-out.md", "walkthrough-linux.md", "walkthrough-windows.md"}

var (
	createsSageRole = regexp.MustCompile(`(?i)CREATE\s+(USER|ROLE)\s+sage_agent\b`)
	createsSage     = regexp.MustCompile(`(?i)CREATE\s+SCHEMA\s+(IF\s+NOT\s+EXISTS\s+)?sage\b`)
	rolePassword    = regexp.MustCompile(`(?i)\bPASSWORD\s+'([^']*)'`)
	sageAgentDSN    = regexp.MustCompile(`postgres(?:ql)?://sage_agent:([^@\s"']*)@`)
)

func readDoc(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(docsGlob), name))
	if err != nil {
		t.Fatalf("read docs/%s: %v", name, err)
	}
	return string(raw)
}

// roleDocs returns every docs/*.md page that creates the sage_agent role.
func roleDocs(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(docsGlob)
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	pages := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if createsSageRole.Match(raw) {
			pages[filepath.Base(p)] = string(raw)
		}
	}
	// quickstart, installation, security, azure and the three try-it pages.
	if len(pages) < 7 {
		t.Fatalf("only %d docs create sage_agent; the docs glob %s is wrong", len(pages), docsGlob)
	}
	return pages
}

// roleBlock returns the body of the first ```sql block on the page that
// creates sage_agent. name is only used to make the error actionable.
func roleBlock(name, doc string) (string, error) {
	rest := strings.ReplaceAll(doc, "\r\n", "\n") // a CRLF checkout reads the same
	for {
		_, after, found := strings.Cut(rest, "```sql\n")
		if !found {
			return "", fmt.Errorf("docs/%s has no ```sql block that creates sage_agent; "+
				"copy the role block from docs/quickstart.md", name)
		}
		body, tail, closed := strings.Cut(after, "```")
		if !closed {
			return "", fmt.Errorf("docs/%s has an unclosed ```sql block", name)
		}
		if createsSageRole.MatchString(body) {
			return body, nil
		}
		rest = tail
	}
}

// blockPassword returns the password the role block sets.
func blockPassword(name, block string) (string, error) {
	m := rolePassword.FindStringSubmatch(block)
	if m == nil {
		return "", fmt.Errorf("docs/%s: the sage_agent role block sets no PASSWORD", name)
	}
	return m[1], nil
}

func TestRoleDocsCreateSageSchema(t *testing.T) {
	for name, doc := range roleDocs(t) {
		if !createsSage.MatchString(doc) {
			t.Errorf("docs/%s creates sage_agent but never creates the sage schema; "+
				"on a stock image the role cannot, so the bootstrap fails", name)
		}
	}
}

func TestRoleDocsTryItPagesCopyQuickstartBlock(t *testing.T) {
	want, err := roleBlock("quickstart.md", readDoc(t, "quickstart.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Pin the canonical block's shape: read-only, owns its schema.
	if !strings.Contains(want, "CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;") {
		t.Fatalf("quickstart role block lost its sage schema: %q", want)
	}
	if strings.Contains(want, "pg_signal_backend") {
		t.Fatalf("quickstart role block grants pg_signal_backend up front: %q", want)
	}
	for _, name := range tryItPages {
		got, err := roleBlock(name, readDoc(t, name))
		if err != nil {
			t.Error(err)
			continue
		}
		if got != want {
			t.Errorf("docs/%s role block differs from docs/quickstart.md:\ngot:\n%s\nwant:\n%s",
				name, got, want)
		}
	}
}

func TestRoleDocsDSNPasswordMatchesRoleBlock(t *testing.T) {
	for name, doc := range roleDocs(t) {
		block, err := roleBlock(name, doc)
		if err != nil {
			t.Error(err)
			continue
		}
		want, err := blockPassword(name, block)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, m := range sageAgentDSN.FindAllStringSubmatch(doc, -1) {
			if m[1] != want {
				t.Errorf("docs/%s connects as sage_agent with password %q, but its role "+
					"block sets %q; the next step fails authentication", name, m[1], want)
			}
		}
	}
}

func TestRoleDocsTryItPagesCountTheDSN(t *testing.T) {
	// Boundary: each try-it page must start pg_sage with exactly one
	// sage_agent DSN, or the password check above passes vacuously.
	for _, name := range tryItPages {
		if n := len(sageAgentDSN.FindAllString(readDoc(t, name), -1)); n != 1 {
			t.Errorf("docs/%s has %d sage_agent DSNs, want 1", name, n)
		}
	}
}

func TestRoleDocsTryItPagesShowFirstLookAndGrantMore(t *testing.T) {
	for _, name := range tryItPages {
		doc := readDoc(t, name)
		for _, want := range []string{"Minute 1", "Getting started", "First look",
			"Grant more", "admin role"} {
			if !strings.Contains(doc, want) {
				t.Errorf("docs/%s does not mention %q", name, want)
			}
		}
		// The pages may name pg_signal_backend as a later grant, as quickstart.md does.
		for _, banned := range []string{"GRANT pg_signal_backend",
			"GRANT CREATE ON SCHEMA public"} {
			if strings.Contains(doc, banned) {
				t.Errorf("docs/%s still grants %q up front; point to Grant more", name, banned)
			}
		}
	}
}

func TestRoleDocsRoleBlockInvalidInput(t *testing.T) {
	cases := []struct {
		label, doc, wantErr string
	}{
		{"empty page", "", "docs/broken.md has no ```sql block"},
		{"role only in bash", "```bash\npsql -c \"CREATE USER sage_agent\"\n```\n",
			"docs/broken.md has no ```sql block"},
		{"sql block without the role", "```sql\nSELECT 1;\n```\n",
			"docs/broken.md has no ```sql block"},
		{"unclosed sql block", "```sql\nCREATE ROLE sage_agent LOGIN;\n",
			"docs/broken.md has an unclosed ```sql block"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			block, err := roleBlock("broken.md", tc.doc)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("roleBlock error = %v, want it to contain %q", err, tc.wantErr)
			}
			if block != "" {
				t.Fatalf("roleBlock returned %q with an error", block)
			}
		})
	}
}

func TestRoleDocsRoleBlockPicksTheRoleBlock(t *testing.T) {
	doc := "```sql\nSELECT 1;\n```\ntext\n```sql\nCREATE ROLE sage_agent LOGIN " +
		"PASSWORD 'p1';\n```\n```sql\nCREATE USER sage_agent PASSWORD 'p2';\n```\n"
	block, err := roleBlock("ok.md", doc)
	if err != nil {
		t.Fatal(err)
	}
	if block != "CREATE ROLE sage_agent LOGIN PASSWORD 'p1';\n" {
		t.Fatalf("roleBlock = %q, want the first block that creates sage_agent", block)
	}
	pw, err := blockPassword("ok.md", block)
	if err != nil || pw != "p1" {
		t.Fatalf("blockPassword = %q, %v; want p1", pw, err)
	}
}

func TestRoleDocsBlockPasswordMissing(t *testing.T) {
	pw, err := blockPassword("nopw.md", "CREATE ROLE sage_agent LOGIN;\n")
	if err == nil || !strings.Contains(err.Error(), "docs/nopw.md") {
		t.Fatalf("blockPassword error = %v, want it to name docs/nopw.md", err)
	}
	if pw != "" {
		t.Fatalf("blockPassword returned %q with an error", pw)
	}
}
