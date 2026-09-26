package config

import (
	"bytes"
	"os"
	"path/filepath"
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
	if len(cfg.Databases) != 0 && cfg.Databases[0].Host == "localhost" {
		t.Fatal("standalone quick start must not silently target localhost")
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

func TestLoad_ExplicitExtensionModeIsKept(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_DATABASE_URL", "postgres://u:p@db.example:5432/app")
	for _, args := range [][]string{{"--mode", "extension"}, nil} {
		if args == nil {
			t.Setenv("SAGE_MODE", "extension")
		}
		cfg, err := Load(args)
		if err != nil {
			t.Fatalf("Load(%v): %v", args, err)
		}
		if cfg.Mode != "extension" {
			t.Fatalf("Load(%v) Mode = %q, want extension", args, cfg.Mode)
		}
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
	if !strings.Contains(text, path) || !strings.Contains(text, "SAGE_TEST_LOAD_UNSET") {
		t.Fatalf("warning output %q lacks path or variable", text)
	}
	if strings.Contains(text, "SAGE_TEST_LOAD_COMMENT") {
		t.Fatalf("comment reference produced a warning: %q", text)
	}
}
