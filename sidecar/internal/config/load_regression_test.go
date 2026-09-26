package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// chdirTemp isolates Load from any config.yaml in the working directory and
// from DSN variables in the developer's shell.
func chdirTemp(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("SAGE_DATABASE_URL", "")
	t.Setenv("SAGE_MODE", "")
	t.Setenv("SAGE_CONFIG_PATH", "")
}

// G10-B01: the documented quick start passes only --pg-url.
func TestLoad_PgURLWithoutModeRunsStandalone(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != "standalone" {
		t.Fatalf("Mode = %q, want standalone", cfg.Mode)
	}
	if len(cfg.Databases) != 1 || cfg.Databases[0].Host != "db.example" ||
		cfg.Databases[0].Database != "app" {
		t.Fatalf("standalone instance identity = %+v, want host db.example db app",
			cfg.Databases)
	}
}

func TestLoad_EnvDatabaseURLWithoutModeRunsStandalone(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_DATABASE_URL", "postgres://u:p@db.example:5432/app")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != "standalone" {
		t.Fatalf("Mode = %q, want standalone", cfg.Mode)
	}
}

// The C extension was removed, so "extension" mode (API over the
// extension's schema) no longer exists. Every source of the setting must
// fail with a hint naming the replacement modes.
func TestLoad_ExtensionModeIsRemoved(t *testing.T) {
	chdirTemp(t)
	check := func(label string, args []string) {
		t.Helper()
		cfg, err := Load(args)
		if err == nil || cfg != nil {
			t.Fatalf("%s: extension mode accepted (cfg=%v)", label, cfg != nil)
		}
		msg := err.Error()
		for _, want := range []string{`"extension"`, "removed", "standalone", "meta-db"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s: error %q lacks %q", label, msg, want)
			}
		}
	}
	check("flag", []string{"--mode", "extension"})
	check("flag with meta-db", []string{
		"--mode", "extension", "--meta-db", "postgres://u:p@meta.example:5432/meta",
	})
	t.Setenv("SAGE_MODE", "extension")
	check("env", nil)
	t.Setenv("SAGE_MODE", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: extension\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("yaml", []string{"--config", path})
}

func TestLoad_NoInputDefaultsToStandalone(t *testing.T) {
	chdirTemp(t)
	if DefaultMode != "standalone" {
		t.Fatalf("DefaultMode = %q, want standalone", DefaultMode)
	}
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != "standalone" || !cfg.IsStandalone() {
		t.Fatalf("Mode = %q, want standalone", cfg.Mode)
	}
	if cfg.Postgres.Host != "localhost" || cfg.Postgres.Port != 5432 {
		t.Fatalf("default target = %s:%d, want localhost:5432",
			cfg.Postgres.Host, cfg.Postgres.Port)
	}
}

// A meta-db deployment with no explicit mode used to carry the "extension"
// label. It is now "meta": neither standalone nor fleet, so the meta-db
// runtime paths are unchanged.
func TestLoad_MetaDBWithoutModeRunsMeta(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--meta-db", "postgres://u:p@meta.example:5432/meta"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeMeta || cfg.IsStandalone() || cfg.IsFleet() {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, ModeMeta)
	}
}

func TestLoad_MetaModeRequiresMetaDB(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--mode", "meta"})
	if err == nil || cfg != nil {
		t.Fatalf("meta mode without a meta database accepted (cfg=%v)", cfg != nil)
	}
	if !strings.Contains(err.Error(), "meta-db") {
		t.Fatalf("error %q does not name --meta-db", err)
	}
	cfg, err = Load([]string{
		"--mode", "meta", "--meta-db", "postgres://u:p@meta.example:5432/meta",
	})
	if err != nil || cfg.Mode != ModeMeta {
		t.Fatalf("explicit meta mode with meta-db: cfg=%v err=%v", cfg, err)
	}
}

func TestLoad_MetaDBWithURLDoesNotInferStandalone(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{
		"--pg-url", "postgres://u:p@db.example:5432/app",
		"--meta-db", "postgres://u:p@meta.example:5432/meta",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode == "standalone" {
		t.Fatal("meta-db deployments must not be promoted to standalone (G5-B25)")
	}
}

func TestLoad_NoURLNoModeKeepsDefault(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != DefaultMode {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, DefaultMode)
	}
}

// G10-B10: a mistyped flag must stop startup instead of using defaults.
func TestLoad_UnknownFlagFails(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-ulr", "postgres://u@h/db"})
	if err == nil || cfg != nil {
		t.Fatalf("Load accepted an unknown flag: cfg=%v err=%v", cfg != nil, err)
	}
	if !strings.Contains(err.Error(), "parse flags") ||
		!strings.Contains(err.Error(), "pg-ulr") {
		t.Fatalf("error %q does not name the bad flag", err)
	}
}

// G10-B02: every shipped example must load under strict parsing.
func TestShippedConfigExamplesLoad(t *testing.T) {
	examples := []string{
		filepath.Join("..", "..", "config.example.yaml"),
		filepath.Join("..", "..", "..", "config.example.yaml"),
	}
	for _, rel := range examples {
		path, err := filepath.Abs(rel)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("shipped example missing: %v", statErr)
			}
			chdirTemp(t)
			cfg, loadErr := Load([]string{"--config", path})
			if loadErr != nil {
				t.Fatalf("example %s does not load: %v", path, loadErr)
			}
			if cfg.ConfigPath != path {
				t.Fatalf("ConfigPath = %q, want %q", cfg.ConfigPath, path)
			}
		})
	}
}

// G10-B09: comments are not configuration; warnings name the real file.
func TestUnexpandedEnvWarnings_IgnoresCommentsAndNamesFile(t *testing.T) {
	t.Setenv("SAGE_TEST_SET_VAR", "value")
	_ = os.Unsetenv("SAGE_TEST_UNSET_VAR")
	_ = os.Unsetenv("SAGE_TEST_COMMENT_VAR")
	raw := strings.Join([]string{
		"# export ${SAGE_TEST_COMMENT_VAR} before starting",
		"llm:",
		"  api_key: ${SAGE_TEST_UNSET_VAR}   # ${SAGE_TEST_COMMENT_VAR}",
		"  endpoint: \"${SAGE_TEST_SET_VAR}\"",
		"#  model: ${SAGE_TEST_COMMENT_VAR}",
	}, "\n")

	got := unexpandedEnvWarnings("/etc/pg_sage/custom.yaml", raw)

	want := []string{`WARNING: config "/etc/pg_sage/custom.yaml" references ` +
		`${SAGE_TEST_UNSET_VAR} but it is not set in the environment`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings = %q, want %q", got, want)
	}
}

func TestUnexpandedEnvWarnings_WrittenDuringLoad(t *testing.T) {
	chdirTemp(t)
	_ = os.Unsetenv("SAGE_TEST_LOAD_UNSET")
	path := filepath.Join(t.TempDir(), "sage.yaml")
	body := "# ${SAGE_TEST_LOAD_COMMENT}\nllm:\n  api_key: ${SAGE_TEST_LOAD_UNSET}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &out
	t.Cleanup(func() { configWarningOutput = previous })

	if _, err := Load([]string{"--config", path}); err != nil {
		t.Fatalf("Load: %v", err)
	}

	text := out.String()
	// The warning quotes the path with %q, so compare the quoted form
	// (Windows paths contain escaped backslashes).
	if !strings.Contains(text, strconv.Quote(path)) ||
		!strings.Contains(text, "SAGE_TEST_LOAD_UNSET") {
		t.Fatalf("warning output %q lacks path or variable", text)
	}
	if strings.Contains(text, "SAGE_TEST_LOAD_COMMENT") {
		t.Fatalf("comment reference produced a warning: %q", text)
	}
}
