package gitops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// cli-02a: the directory loader reads policy-sets/, principals/ and
// grants/, checks every file's kind and schema version, and reports
// every problem with its file and line. Loading is pure (no database),
// so there is no concurrent-access test here; the reconcile's are in
// reconcile_db_test.go.

const setProd = `kind: PolicySet
schema_version: 1
name: prod-strict
priority: 50
selectors:
  - tags: {tier: prod}
`

const setStage = `kind: PolicySet
schema_version: 1
name: stage
priority: 40
selectors:
  - tags: {tier: stage}
`

const principalBot = `kind: Principal
schema_version: 1
name: ci-bot
profile: readonly-analyst
env_ceiling: dev
`

const grantsTwo = `kind: DatabaseGrants
schema_version: 1
grants:
  - user: alice@example.com
    database: orders
    role: viewer
  - user: Bob@Example.com
    database: orders
    role: operator
`

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func happyTree(t *testing.T) string {
	return writeTree(t, map[string]string{
		"policy-sets/prod.yaml": setProd, "policy-sets/stage.yml": setStage,
		"principals/ci-bot.yaml": principalBot, "grants/orders.yaml": grantsTwo,
		"README.md": "not read", "policy-sets/notes.txt": "ignored",
	})
}

func TestLoad_HappyPathReadsEveryKind(t *testing.T) {
	dir, err := Load(happyTree(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(dir.Sets) != 2 || dir.Sets[0].File.Name != "prod-strict" ||
		dir.Sets[1].File.Name != "stage" || dir.Sets[0].File.Priority != 50 {
		t.Fatalf("sets = %+v", dir.Sets)
	}
	if dir.Sets[0].Path != "policy-sets/prod.yaml" || len(dir.Sets[0].Hash) != 64 {
		t.Fatalf("set source = %q %q", dir.Sets[0].Path, dir.Sets[0].Hash)
	}
	if len(dir.Principals) != 1 || dir.Principals[0].Name != "ci-bot" ||
		dir.Principals[0].Profile != "readonly-analyst" ||
		dir.Principals[0].EnvCeiling != "dev" {
		t.Fatalf("principals = %+v", dir.Principals)
	}
	if len(dir.Grants) != 2 || dir.Grants[0].User != "alice@example.com" ||
		dir.Grants[1].User != "bob@example.com" || dir.Grants[1].Role != "operator" ||
		dir.Grants[1].Line != 7 {
		t.Fatalf("grants = %+v", dir.Grants)
	}
	if len(dir.Hash) != 64 {
		t.Fatalf("directory hash = %q", dir.Hash)
	}
	want := "config:policy-sets/prod.yaml@" + dir.Sets[0].Hash
	if got := dir.Sets[0].Actor(); got != want {
		t.Fatalf("actor = %q, want %q", got, want)
	}
}

func TestLoad_HashIsStableAndFollowsContent(t *testing.T) {
	a, err := Load(happyTree(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(happyTree(t))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("same content, different hashes %s %s", a.Hash, b.Hash)
	}
	root := happyTree(t)
	if err := os.WriteFile(filepath.Join(root, "principals", "ci-bot.yaml"),
		[]byte(strings.Replace(principalBot, "dev", "branch", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hash == a.Hash {
		t.Fatal("a changed file kept the directory hash")
	}
}

func problemsOf(t *testing.T, err error) []Problem {
	t.Helper()
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v (%T), want *InvalidError", err, err)
	}
	return invalid.Problems
}

func wantProblem(t *testing.T, err error, file string, line int, text string) {
	t.Helper()
	for _, p := range problemsOf(t, err) {
		if p.File == file && p.Line == line && strings.Contains(p.Message, text) {
			if !strings.Contains(err.Error(), file) {
				t.Fatalf("error text %q does not name %s", err.Error(), file)
			}
			return
		}
	}
	t.Fatalf("no problem %s:%d %q in %v", file, line, text, err)
}

func TestLoad_InvalidInputNamesFileAndLine(t *testing.T) {
	cases := []struct {
		name, file, body, text string
		line                   int
	}{
		{"unknown kind", "principals/x.yaml", "kind: Robot\nschema_version: 1\n",
			"unknown kind", 1},
		{"kind in the wrong directory", "grants/x.yaml", principalBot,
			"belongs in principals/", 1},
		{"missing kind", "principals/x.yaml", "name: ci-bot\n", "kind", 1},
		{"schema version", "principals/x.yaml",
			strings.Replace(principalBot, "schema_version: 1", "schema_version: 2", 1),
			"schema_version", 2},
		{"selector that does not parse", "policy-sets/x.yaml",
			strings.Replace(setProd, "  - tags: {tier: prod}", "  - colour: blue", 1),
			"invalid policy set", 6},
		{"unknown profile", "principals/x.yaml",
			strings.Replace(principalBot, "readonly-analyst", "root", 1), "profile", 4},
		{"bad env ceiling", "principals/x.yaml",
			strings.Replace(principalBot, "env_ceiling: dev", "env_ceiling: moon", 1),
			"env_ceiling", 5},
		{"unknown key", "principals/x.yaml", principalBot + "token: abc\n",
			"unknown key \"token\"", 6},
		{"bad grant role", "grants/x.yaml",
			strings.Replace(grantsTwo, "role: viewer", "role: admin", 1), "role", 4},
		{"grant without database", "grants/x.yaml",
			strings.Replace(grantsTwo, "    database: orders\n    role: viewer\n",
				"    role: viewer\n", 1), "database", 4},
		{"not YAML", "principals/x.yaml", "kind: [\n", "YAML", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTree(t, map[string]string{tc.file: tc.body}))
			wantProblem(t, err, tc.file, tc.line, tc.text)
		})
	}
}

func TestLoad_EveryProblemIsReported(t *testing.T) {
	root := writeTree(t, map[string]string{
		"principals/a.yaml": "kind: Robot\nschema_version: 1\n",
		"principals/b.yaml": strings.Replace(principalBot, "readonly-analyst", "root", 1),
		"principals/c.yaml": principalBot, "principals/d.yaml": principalBot,
	})
	_, err := Load(root)
	problems := problemsOf(t, err)
	if len(problems) != 3 {
		t.Fatalf("problems = %v, want the kind, the profile and the duplicate", problems)
	}
	wantProblem(t, err, "principals/d.yaml", 3, "also in principals/c.yaml")
}

func TestLoad_DuplicateSetAndGrantAcrossFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"policy-sets/a.yaml": setProd, "policy-sets/b.yaml": setProd,
		"grants/a.yaml": grantsTwo, "grants/b.yaml": grantsTwo,
	})
	_, err := Load(root)
	wantProblem(t, err, "policy-sets/b.yaml", 3, "also in policy-sets/a.yaml")
	wantProblem(t, err, "grants/b.yaml", 4, "also in grants/a.yaml")
}

func TestLoad_MissingDirectoryIsDistinguishable(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, ErrDirectoryMissing) {
		t.Fatalf("missing directory error = %v, want ErrDirectoryMissing", err)
	}
	file := filepath.Join(t.TempDir(), "file.yaml")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil || errors.Is(err, ErrDirectoryMissing) {
		t.Fatalf("a file as the directory = %v, want a not-a-directory error", err)
	}
}

func TestLoad_EmptyAndNilInputs(t *testing.T) {
	dir, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("empty directory: %v", err)
	}
	if len(dir.Sets)+len(dir.Principals)+len(dir.Grants) != 0 || len(dir.Hash) != 64 {
		t.Fatalf("empty directory loaded %+v", dir)
	}
	if _, err := Load(""); err == nil {
		t.Fatal("an empty path loaded")
	}
	root := writeTree(t, map[string]string{"principals/empty.yaml": ""})
	_, err = Load(root)
	wantProblem(t, err, "principals/empty.yaml", 1, "empty")
}

func TestLoad_FileSizeBoundary(t *testing.T) {
	pad := func(n int) string {
		body := principalBot
		return body + "#" + strings.Repeat("x", n-len(body)-2) + "\n"
	}
	at := pad(MaxFileBytes)
	if len(at) != MaxFileBytes {
		t.Fatalf("fixture is %d bytes", len(at))
	}
	if _, err := Load(writeTree(t, map[string]string{"principals/a.yaml": at})); err != nil {
		t.Fatalf("a 1 MiB file was refused: %v", err)
	}
	over := pad(MaxFileBytes + 1)
	_, err := Load(writeTree(t, map[string]string{"principals/a.yaml": over}))
	wantProblem(t, err, "principals/a.yaml", 0, "over 1 MiB")
}

func TestLoad_SetDocumentMustParse(t *testing.T) {
	body := setProd + "document:\n  profile: unattended\n  body: {serialize_mode: park}\n"
	_, err := Load(writeTree(t, map[string]string{"policy-sets/p.yaml": body}))
	wantProblem(t, err, "policy-sets/p.yaml", 7, "document")
}

// A document's profile is staffed or unattended; anything else would only
// fail at apply time, so the loader refuses it with its line.
func TestLoad_SetDocumentProfileMustBeKnown(t *testing.T) {
	raw, err := policy.MarshalDocument(policy.UnattendedProfile())
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, profile := range []string{"", "autonomous"} {
		body := setProd + "document:\n  profile: \"" + profile + "\"\n  body: " + doc + "\n"
		_, err := Load(writeTree(t, map[string]string{"policy-sets/p.yaml": body}))
		wantProblem(t, err, "policy-sets/p.yaml", 7, "profile")
	}
}
