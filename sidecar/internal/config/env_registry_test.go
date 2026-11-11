package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// config-08: every SAGE_* variable the sidecar reads is in the registry,
// and docs/configuration.md carries the table rendered from it.
//
// No concurrent-access tests: the registry is an immutable package value
// and the renderer is a pure function of it.

// envRegistrySources are the trees (relative to this package) whose non-test
// Go sources must only read registered variables (ci-10 widened them from
// internal/config and cmd/pg_sage_sidecar to every runtime and contributor
// reader).
var envRegistrySources = []string{
	filepath.Join("..", "..", "internal"),
	filepath.Join("..", "..", "cmd"),
	filepath.Join("..", "..", "sre-bench"),
	filepath.Join("..", "..", "safety-bench"),
}

var sageEnvLiteral = regexp.MustCompile(`"(SAGE_[A-Z0-9_]+)"`)

// sourceEnvLiterals returns the sorted SAGE_* literals of the sources,
// leaving out the registry file itself.
func sourceEnvLiterals(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, root := range envRegistrySources {
		files, err := envSourceFiles(root)
		if err != nil {
			t.Fatalf("list Go sources under %s: %v", root, err)
		}
		if len(files) == 0 {
			t.Fatalf("no Go sources under %s", root)
		}
		for _, file := range files {
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			for _, m := range sageEnvLiteral.FindAllStringSubmatch(string(body), -1) {
				seen[m[1]] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// envSourceFiles lists the non-test Go files under root, skipping testdata
// trees and the registry file itself.
func envSourceFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if !strings.HasSuffix(base, ".go") || strings.HasSuffix(base, "_test.go") ||
			base == "env_registry.go" {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

func TestEnvRegistryCoversEverySourceLiteral(t *testing.T) {
	literals := sourceEnvLiterals(t)
	if len(literals) < 50 {
		t.Fatalf("found only %d SAGE_* literals; the scan is broken", len(literals))
	}
	for _, want := range []string{"SAGE_SUPERVISED", "SAGE_MCP_ENABLED", "SAGE_CONFIG_PATH",
		"SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE", "SAGE_AZURE_SERVER_NAME"} {
		if !containsString(literals, want) {
			t.Errorf("scan missed %s", want)
		}
	}
	if gaps := envRegistryGaps(EnvVars(), literals); len(gaps) != 0 {
		t.Fatalf("SAGE_* variables read but not in env_registry.go: %v; add them "+
			"there, then run `go run ./cmd/gen_config_meta -env-only -env-docs "+
			"../docs/configuration.md`", gaps)
	}
}

// Removing any one entry from the registry must make the drift check fail
// for exactly that variable.
func TestEnvRegistryGapsDetectsARemovedEntry(t *testing.T) {
	literals := sourceEnvLiterals(t)
	registry := EnvVars()
	for i, entry := range registry {
		if !containsString(literals, entry.Name) {
			continue
		}
		trimmed := append(append([]EnvVar{}, registry[:i]...), registry[i+1:]...)
		gaps := envRegistryGaps(trimmed, literals)
		if !containsString(gaps, entry.Name) {
			t.Errorf("removing %s from the registry left gaps %v", entry.Name, gaps)
		}
	}
}

func TestEnvRegistryGapsHandlesFileVariants(t *testing.T) {
	registry := []EnvVar{{Name: "SAGE_A", File: true, Doc: "a"}, {Name: "SAGE_B", Doc: "b"}}
	got := envRegistryGaps(registry, []string{"SAGE_A_FILE", "SAGE_B_FILE", "SAGE_C"})
	if strings.Join(got, ",") != "SAGE_B_FILE,SAGE_C" {
		t.Fatalf("gaps = %v, want [SAGE_B_FILE SAGE_C]", got)
	}
	if got := envRegistryGaps(nil, nil); len(got) != 0 {
		t.Fatalf("empty inputs gave gaps %v", got)
	}
}

func TestEnvRegistryEntriesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range EnvVars() {
		if !strings.HasPrefix(entry.Name, "SAGE_") || strings.HasSuffix(entry.Name, "_FILE") {
			t.Errorf("%q: names start with SAGE_ and never end in _FILE "+
				"(set File instead)", entry.Name)
		}
		if seen[entry.Name] {
			t.Errorf("%s registered twice", entry.Name)
		}
		seen[entry.Name] = true
		if entry.Default == "" || len(entry.Doc) < 15 || strings.Contains(entry.Doc, "|") {
			t.Errorf("%s: default %q / doc %q must be set, and the doc a sentence "+
				"without a pipe", entry.Name, entry.Default, entry.Doc)
		}
	}
}

// Every secret pg_sage reads through NAME_FILE is registered with File.
func TestEnvRegistryMarksEverySecretFileVariant(t *testing.T) {
	byName := map[string]EnvVar{}
	for _, entry := range EnvVars() {
		byName[entry.Name] = entry
	}
	for _, name := range SecretEnvNames() {
		if entry, ok := byName[name]; !ok || !entry.File {
			t.Errorf("%s accepts %s_FILE but the registry entry (%v) says not",
				name, name, ok)
		}
	}
}

func TestEnvVarsReturnsACopy(t *testing.T) {
	first := EnvVars()
	first[0].Name = "SAGE_MUTATED"
	if EnvVars()[0].Name == "SAGE_MUTATED" {
		t.Fatal("EnvVars exposes the registry for mutation")
	}
}

func TestEnvVarsMarkdownRendersEveryEntry(t *testing.T) {
	doc := EnvVarsMarkdown()
	if !strings.HasPrefix(doc, "| Variable | Default | Description |\n|---|---|---|\n") {
		t.Fatalf("table header = %q", strings.SplitN(doc, "\n", 3)[:2])
	}
	for _, entry := range EnvVars() {
		if entry.Kind != EnvRuntime {
			continue // ci-10: contributor entries render into CONTRIBUTING.md
		}
		row := "| `" + entry.Name + "`"
		if entry.File {
			row += " / `_FILE`"
		}
		row += " | " + entry.Default + " | " + entry.Doc + " |\n"
		if !strings.Contains(doc, row) {
			t.Errorf("table lacks row %q", row)
		}
	}
	if !strings.Contains(doc, "| `SAGE_PROMETHEUS_PORT` | `8187` | ") ||
		!strings.Contains(doc, "keeps the host") {
		t.Error("SAGE_PROMETHEUS_PORT row does not say it keeps the host")
	}
}

// The generated section of docs/configuration.md matches the registry.
func TestConfigurationDocsEnvTableMatchesRegistry(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "configuration.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got, err := EnvDocsSection(strings.ReplaceAll(string(body), "\r\n", "\n"))
	if err != nil {
		t.Fatalf("docs/configuration.md: %v", err)
	}
	if got != EnvVarsMarkdown() {
		t.Fatal("docs/configuration.md env table drifted from env_registry.go; " +
			"regenerate with `go run ./cmd/gen_config_meta -env-only " +
			"-env-docs ../docs/configuration.md`")
	}
}

func TestReplaceEnvDocsSection(t *testing.T) {
	doc := "intro\n" + EnvDocsStart + "\nold\n" + EnvDocsEnd + "\ntail\n"
	got, err := ReplaceEnvDocsSection(doc)
	if err != nil {
		t.Fatalf("ReplaceEnvDocsSection: %v", err)
	}
	want := "intro\n" + EnvDocsStart + "\n" + EnvVarsMarkdown() + EnvDocsEnd + "\ntail\n"
	if got != want {
		t.Fatalf("replaced doc = %q, want %q", got, want)
	}
	if again, err := ReplaceEnvDocsSection(got); err != nil || again != got {
		t.Fatalf("second replace changed the doc or failed: %v", err)
	}
	for _, bad := range []string{"no markers", EnvDocsStart + "\nno end",
		EnvDocsEnd + "\n" + EnvDocsStart + "\n"} {
		if _, err := ReplaceEnvDocsSection(bad); err == nil {
			t.Errorf("ReplaceEnvDocsSection(%q) succeeded, want a marker error", bad)
		}
		if _, err := EnvDocsSection(bad); err == nil {
			t.Errorf("EnvDocsSection(%q) succeeded, want a marker error", bad)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// envRegistryGaps returns the literals no registry entry covers, sorted; a
// NAME_FILE literal is covered by a NAME entry with File set.
func envRegistryGaps(registry []EnvVar, literals []string) []string {
	covered := map[string]bool{}
	for _, entry := range registry {
		covered[entry.Name] = true
		if entry.File {
			covered[entry.Name+secretFileSuffix] = true
		}
	}
	var gaps []string
	for _, name := range literals {
		if !covered[name] {
			gaps = append(gaps, name)
		}
	}
	sort.Strings(gaps)
	return gaps
}
