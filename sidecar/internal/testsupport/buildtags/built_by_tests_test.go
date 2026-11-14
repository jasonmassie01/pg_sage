package buildtags

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// No concurrent access or state-transition tests: TagsBuiltByTests is a pure
// read of the files it is given.

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Happy path: a tag whose test file still passes it is reported, sorted.
func TestTagsBuiltByTestsReportsTagsTheirTestStillBuilds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "e2e/a_test.go", "package e2e\nconst tag = \"zz_tag\"\n")
	writeFile(t, root, "e2e/b_test.go", "package e2e\nconst tag = \"aa_tag\"\n")
	got, err := TagsBuiltByTests(root, map[string]string{
		"zz_tag": "e2e/a_test.go", "aa_tag": "e2e/b_test.go"})
	if err != nil {
		t.Fatalf("TagsBuiltByTests: %v", err)
	}
	if want := []string{"aa_tag", "zz_tag"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %q, want %q", got, want)
	}
}

// Error propagation: the exemption cannot outlive the test that compiles
// the tag; a missing file is an error naming the tag and the file.
func TestTagsBuiltByTestsRefusesAMissingTestFile(t *testing.T) {
	_, err := TagsBuiltByTests(t.TempDir(), map[string]string{"zz_tag": "e2e/gone_test.go"})
	if err == nil {
		t.Fatal("a missing test file was accepted")
	}
	for _, want := range []string{"zz_tag", "e2e/gone_test.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// Invalid input: a test file that no longer names the tag voids the exemption.
func TestTagsBuiltByTestsRefusesAFileThatNoLongerNamesTheTag(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "e2e/a_test.go", "package e2e\nconst tag = \"other_tag\"\n")
	_, err := TagsBuiltByTests(root, map[string]string{"zz_tag": "e2e/a_test.go"})
	if err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Fatalf("err = %v, want a file that no longer names zz_tag refused", err)
	}
}

// Nil/empty: no exemptions means no extra vetted tags and no error.
func TestTagsBuiltByTestsEmpty(t *testing.T) {
	for _, built := range []map[string]string{nil, {}} {
		got, err := TagsBuiltByTests(t.TempDir(), built)
		if err != nil || len(got) != 0 {
			t.Fatalf("TagsBuiltByTests(%v) = %q, %v; want none", built, got, err)
		}
	}
}

// Integration against the repository: every shipped exemption is real, and
// the assumption tag is exempt, never listed in the vet step (executor-10).
func TestShippedTagsBuiltByTestsAreReal(t *testing.T) {
	got, err := TagsBuiltByTests(moduleRoot(t), BuiltByTests)
	if err != nil {
		t.Fatalf("shipped exemption: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"sage_e2e_assume_sqlast"}) {
		t.Fatalf("exempt tags = %q, want only sage_e2e_assume_sqlast", got)
	}
}
