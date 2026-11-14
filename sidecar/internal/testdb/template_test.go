package testdb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ci-15 unit tests: template names, the template hash and the template0
// fallback. The server side is in template_db_test.go. No concurrent-access
// tests here: these functions read files and their arguments only.

// fakeRoot is a module root holding a checksum file (when checksums is not
// "-") and one schema source file.
func fakeRoot(t *testing.T, checksums, source string) string {
	t.Helper()
	root := t.TempDir()
	schemaDir := filepath.Join(root, "internal", "schema")
	if err := os.MkdirAll(filepath.Join(schemaDir, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if checksums != "-" {
		path := filepath.Join(schemaDir, "testdata", "migration_checksums.txt")
		if err := os.WriteFile(path, []byte(checksums), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{"bootstrap.go": source, "x_test.go": "package schema // test"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(schemaDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestValidateTemplateDatabaseName(t *testing.T) {
	good := templatePrefix + "0123456789abcdef"
	if err := validateTemplateDatabaseName(good); err != nil {
		t.Fatalf("%s refused: %v", good, err)
	}
	for _, bad := range []string{
		"", "template0", "template1", "postgres", templatePrefix,
		templatePrefix + "0123456789ABCDEF",
		templatePrefix + "0123456789abcde",
		templatePrefix + "0123456789abcdef0",
		templatePrefix + "0123456789abcdef; DROP DATABASE postgres",
		fixtureDatabaseName("internal/schema", 42),
	} {
		err := validateTemplateDatabaseName(bad)
		if err == nil || !strings.Contains(err.Error(), "refusing non-template database name") {
			t.Errorf("validateTemplateDatabaseName(%q) = %v, want a refusal", bad, err)
		}
	}
}

// The fixture pattern and the template pattern never accept each other's
// names: dropFixture can never drop a template, nor the sweep a fixture.
func TestTemplateAndFixtureNamesAreDisjoint(t *testing.T) {
	tpl := templatePrefix + "0123456789abcdef"
	if err := validateFixtureDatabaseName(tpl); err == nil {
		t.Fatalf("the fixture pattern accepts template %s", tpl)
	}
	if err := dropFixture("postgres://u@127.0.0.1:1/postgres", tpl); err == nil ||
		!strings.Contains(err.Error(), "refusing non-fixture database name") {
		t.Fatalf("dropFixture(%s) = %v, want a refusal before connecting", tpl, err)
	}
	if err := dropTemplate(context.Background(), nil, fixtureDatabaseName("x", 1)); err == nil ||
		!strings.Contains(err.Error(), "refusing non-template database name") {
		t.Fatalf("dropTemplate(fixture) = %v, want a refusal", err)
	}
}

func TestCreateFixtureFromRefusesAnUnknownTemplate(t *testing.T) {
	name := fixtureDatabaseName("internal/x", 7)
	for _, tpl := range []string{"template1", "postgres", "", "pgsage_tpl_zz"} {
		_, err := createFixtureFrom("postgres://u@127.0.0.1:1/postgres", name, "designated", tpl)
		if err == nil || !strings.Contains(err.Error(), "refusing non-template database name") {
			t.Errorf("createFixtureFrom(template %q) = %v, want a refusal", tpl, err)
		}
	}
}

func TestTemplateHash_IsStableAndFollowsTheSchema(t *testing.T) {
	a1, err := templateHash(fakeRoot(t, "stepA 1111\n", "package schema // v1"))
	if err != nil {
		t.Fatalf("templateHash: %v", err)
	}
	a2, _ := templateHash(fakeRoot(t, "stepA 1111\n", "package schema // v1"))
	b, _ := templateHash(fakeRoot(t, "stepA 1111\nstepB 2222\n", "package schema // v1"))
	c, _ := templateHash(fakeRoot(t, "stepA 1111\n", "package schema // v2"))
	if a1 != a2 {
		t.Fatalf("the same inputs hash to %s and %s", a1, a2)
	}
	if a1 == b || a1 == c || b == c {
		t.Fatalf("a new checksum or schema source kept the hash: %s %s %s", a1, b, c)
	}
	if err := validateTemplateDatabaseName(templatePrefix + a1); err != nil {
		t.Fatalf("hash %q does not make a template name: %v", a1, err)
	}
}

// Test files are not part of the hash: editing a schema test reuses the
// template.
func TestTemplateHash_IgnoresTestFiles(t *testing.T) {
	root := fakeRoot(t, "stepA 1111\n", "package schema")
	before, _ := templateHash(root)
	path := filepath.Join(root, "internal", "schema", "x_test.go")
	if err := os.WriteFile(path, []byte("package schema // edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after, _ := templateHash(root); after != before {
		t.Fatalf("a test-file edit changed the hash %s -> %s", before, after)
	}
}

func TestTemplateHash_NoChecksumFileMeansNoTemplate(t *testing.T) {
	for name, root := range map[string]string{
		"missing": fakeRoot(t, "-", "package schema"),
		"empty":   fakeRoot(t, "", "package schema"),
		"no root": "",
	} {
		hash, err := templateHash(root)
		if !errors.Is(err, errNoChecksums) || hash != "" {
			t.Errorf("%s: templateHash = %q, %v; want errNoChecksums", name, hash, err)
		}
	}
}

func TestModuleRootFindsGoMod(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("moduleRoot %s has no go.mod: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, checksumFile)); err != nil {
		t.Fatalf("moduleRoot %s has no %s: %v", root, checksumFile, err)
	}
}

func captureDiag(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := diag
	diag = func() io.Writer { return &buf }
	t.Cleanup(func() { diag = saved })
	return &buf
}

// Nil/empty: without a checksum file the fixtures start from template0,
// said once per process, and the bootstrap never runs.
func TestChooseTemplate_FallsBackToTemplate0OnceLogged(t *testing.T) {
	logs := captureDiag(t)
	noTemplateOnce = newOnce()
	t.Cleanup(func() { noTemplateOnce = newOnce() })
	ran := false
	boot := func(context.Context, *pgxpool.Pool) error { ran = true; return nil }
	root := fakeRoot(t, "-", "package schema")
	for range 2 {
		got, err := chooseTemplate("postgres://u@127.0.0.1:1/postgres", root, boot)
		if err != nil || got != "template0" {
			t.Fatalf("chooseTemplate = %q, %v; want template0", got, err)
		}
	}
	if ran {
		t.Fatal("the bootstrap ran without a template")
	}
	if n := strings.Count(logs.String(), "no fixture template"); n != 1 {
		t.Fatalf("fallback logged %d times, want once: %q", n, logs.String())
	}
}

// Nil bootstrap: a package that did not opt in uses template0 silently.
func TestChooseTemplate_NilBootstrapIsTemplate0(t *testing.T) {
	logs := captureDiag(t)
	got, err := chooseTemplate("postgres://u@127.0.0.1:1/postgres",
		fakeRoot(t, "stepA 1\n", "package schema"), nil)
	if err != nil || got != "template0" || logs.Len() != 0 {
		t.Fatalf("chooseTemplate(nil) = %q, %v, log %q", got, err, logs.String())
	}
}

func TestFixtureTemplateIsEmptyBeforeAnyRun(t *testing.T) {
	saved := FixtureTemplate()
	setFixtureTemplate("")
	t.Cleanup(func() { setFixtureTemplate(saved) })
	if got := FixtureTemplate(); got != "" {
		t.Fatalf("FixtureTemplate() = %q before a run", got)
	}
}
