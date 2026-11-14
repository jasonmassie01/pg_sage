package buildtags

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// No concurrent access tests: every function here is a pure read of its
// arguments or of a directory tree and keeps no shared state.
// No state transitions: the contract has no modes; it is a set comparison.
// No PostgreSQL integration: the contract reads source and workflow files
// only. TestEveryTagIsVetInCI is the integration check against the repo.

// TestEveryTagIsVetInCI is the ci-04 contract: every custom build tag used
// under sidecar/ must be compiled by CI's tagged vet step, so a tagged
// suite cannot rot unnoticed again (providerlive did, 2026-10-04).
func TestEveryTagIsVetInCI(t *testing.T) {
	root := moduleRoot(t)
	used, err := ScanTree(root)
	if err != nil {
		t.Fatalf("scan build constraints under %s: %v", root, err)
	}
	for _, want := range []string{"integration", "e2e", "providerlive"} {
		if _, ok := used[want]; !ok {
			t.Fatalf("scan missed tag %q; found %v", want, sortedKeys(used))
		}
	}
	ci, err := os.ReadFile(filepath.Join(root, "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	vetted, err := VetTags(ci)
	if err != nil {
		t.Fatalf("ci.yml: %v", err)
	}
	built, err := TagsBuiltByTests(root, BuiltByTests)
	if err != nil {
		t.Fatalf("tags compiled by a test: %v", err)
	}
	vetted = append(vetted, built...)
	if missing := MissingTags(used, vetted); len(missing) > 0 {
		for _, tag := range missing {
			t.Errorf("tag %q (used in %s) is not in the CI vet -tags= list %v",
				tag, used[tag][0], vetted)
		}
	}
}

func TestCustomTagsBoundaries(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{"//go:build integration || e2e", []string{"e2e", "integration"}},
		{"//go:build !cgo", nil},
		{"//go:build cgo", nil},
		{"//go:build !unix && !windows", nil},
		{"//go:build linux && amd64", nil},
		{"//go:build go1.21 && providerlive", []string{"providerlive"}},
		{"//go:build ignore", nil},
		{"//go:build !perfgate", []string{"perfgate"}},
		{"//go:build (e2e && darwin) || (arm64 && integration)", []string{"e2e", "integration"}},
		{"// +build cloudlive", []string{"cloudlive"}},
	}
	for _, tc := range cases {
		got, err := CustomTags(tc.line)
		if err != nil {
			t.Fatalf("CustomTags(%q): %v", tc.line, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CustomTags(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestCustomTagsInvalidConstraint(t *testing.T) {
	_, err := CustomTags("//go:build (integration")
	if err == nil || !strings.Contains(err.Error(), "(integration") {
		t.Fatalf("malformed constraint error = %v, want one naming the line", err)
	}
}

func TestVetTagsEmptyWorkflow(t *testing.T) {
	for name, ci := range map[string][]byte{
		"nil":        nil,
		"empty":      {},
		"untagged":   []byte("      - name: Go vet\n        run: go vet ./...\n"),
		"empty list": []byte("        run: go vet -tags= ./...\n"),
	} {
		got, err := VetTags(ci)
		if err == nil || !strings.Contains(err.Error(), "no tagged vet step") {
			t.Errorf("%s: VetTags = %v, %v; want a no tagged vet step error", name, got, err)
		}
	}
}

func TestVetTagsParsesStep(t *testing.T) {
	ci := []byte("  - name: Go vet (tagged)\n" +
		"    run: go vet -tags=integration,e2e,perfgate ./...\n")
	got, err := VetTags(ci)
	if err != nil {
		t.Fatalf("VetTags: %v", err)
	}
	want := []string{"integration", "e2e", "perfgate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("VetTags = %v, want %v", got, want)
	}
}

func TestVetTagsSpaceSeparatedFlag(t *testing.T) {
	got, err := VetTags([]byte("run: go vet -tags integration,cloudlive ./...\n"))
	if err != nil {
		t.Fatalf("VetTags: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"integration", "cloudlive"}) {
		t.Fatalf("VetTags = %v", got)
	}
}

func TestMissingTags(t *testing.T) {
	used := map[string][]string{
		"integration": {"a_test.go"}, "providerlive": {"b_test.go"}, "e2e": {"c_test.go"},
	}
	got := MissingTags(used, []string{"integration"})
	if !reflect.DeepEqual(got, []string{"e2e", "providerlive"}) {
		t.Fatalf("MissingTags = %v, want [e2e providerlive]", got)
	}
	if got := MissingTags(used, []string{"e2e", "integration", "providerlive"}); got != nil {
		t.Fatalf("MissingTags with full list = %v, want nil", got)
	}
	if got := MissingTags(nil, nil); got != nil {
		t.Fatalf("MissingTags(nil, nil) = %v, want nil", got)
	}
}

func TestScanTreeFixture(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/a_test.go", "//go:build integration || e2e\n\npackage a\n")
	write(t, root, "b/b.go", "// Package b.\n//go:build !cgo\n\npackage b\n")
	write(t, root, "c/c.go", "package c\n\n//go:build late\n")
	write(t, root, "testdata/t.go", "//go:build fixtureonly\n\npackage t\n")
	write(t, root, "web/node_modules/x/x.go", "//go:build nodeonly\n\npackage x\n")
	write(t, root, "d/notes.txt", "//go:build textonly\n")
	write(t, root, "nested/go.mod", "module example.com/nested\n")
	write(t, root, "nested/n/n_test.go", "//go:build othermodule\n\npackage n\n")
	got, err := ScanTree(root)
	if err != nil {
		t.Fatalf("ScanTree: %v", err)
	}
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, []string{"e2e", "integration"}) {
		t.Fatalf("ScanTree tags = %v, want [e2e integration]", keys)
	}
	if files := got["e2e"]; len(files) != 1 || filepath.Base(files[0]) != "a_test.go" {
		t.Fatalf("e2e files = %v, want [a/a_test.go]", files)
	}
}

func TestScanTreeErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := ScanTree(missing); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("ScanTree(missing) error = %v, want one naming the path", err)
	}
	root := t.TempDir()
	write(t, root, "bad/bad.go", "//go:build (integration\n\npackage bad\n")
	if _, err := ScanTree(root); err == nil || !strings.Contains(err.Error(), "bad.go") {
		t.Fatalf("ScanTree(malformed) error = %v, want one naming bad.go", err)
	}
}

func TestScanTreeEmpty(t *testing.T) {
	got, err := ScanTree(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("ScanTree(empty dir) = %v, %v; want no tags and no error", got, err)
	}
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
