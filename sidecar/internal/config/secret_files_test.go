package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GR-11: pg_sage reads its own secrets from NAME_FILE as well as NAME.

func writeSecretFile(t *testing.T, body string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func clearSecretEnv(t *testing.T) {
	t.Helper()
	for _, name := range SecretEnvNames() {
		t.Setenv(name, "")
		t.Setenv(name+"_FILE", "")
	}
}

// secretFields maps every secret variable Load applies to the field it sets.
var secretFields = map[string]func(*Config) string{
	"SAGE_DATABASE_URL":          func(c *Config) string { return c.Postgres.DatabaseURL },
	"SAGE_PG_PASSWORD":           func(c *Config) string { return c.Postgres.Password },
	"SAGE_META_DB":               func(c *Config) string { return c.MetaDB },
	"SAGE_ENCRYPTION_KEY":        func(c *Config) string { return c.EncryptionKey },
	"SAGE_LLM_API_KEY":           func(c *Config) string { return c.LLM.APIKey },
	"SAGE_OPTIMIZER_LLM_API_KEY": func(c *Config) string { return c.LLM.OptimizerLLM.APIKey },
	"SAGE_OAUTH_CLIENT_SECRET":   func(c *Config) string { return c.OAuth.ClientSecret },
	"SAGE_CLONE_DLE_TOKEN":       func(c *Config) string { return c.Clone.DLEToken },
	"SAGE_API_KEY":               func(c *Config) string { return c.APIKey },
}

func TestSecretFile_EverySecretVariableAcceptsAFile(t *testing.T) {
	for name, field := range secretFields {
		t.Run(name, func(t *testing.T) {
			chdirTemp(t)
			clearSecretEnv(t)
			value := "postgres://u:from-file-" + strings.ToLower(name) + "@h/db"
			t.Setenv(name+"_FILE", writeSecretFile(t, value+"\n", 0o600))
			cfg, err := Load(nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := field(cfg); got != value {
				t.Fatalf("%s from file = %q, want %q", name, got, value)
			}
		})
	}
}

func TestSecretEnvNames_CoverEveryAppliedField(t *testing.T) {
	names := map[string]bool{}
	for _, name := range SecretEnvNames() {
		names[name] = true
	}
	for name := range secretFields {
		if !names[name] {
			t.Errorf("%s is applied by Load but not listed by SecretEnvNames", name)
		}
	}
	if !names["SAGE_SUPABASE_OBSERVABILITY_TOKEN"] {
		t.Error("the provider observability token is read from env but not listed")
	}
}

func TestSecretFile_EnvAndFileBothSetIsAnError(t *testing.T) {
	chdirTemp(t)
	clearSecretEnv(t)
	t.Setenv("SAGE_LLM_API_KEY", "env-value-xyz")
	t.Setenv("SAGE_LLM_API_KEY_FILE", writeSecretFile(t, "file-value-xyz", 0o600))
	_, err := Load(nil)
	if err == nil {
		t.Fatal("Load accepted SAGE_LLM_API_KEY and SAGE_LLM_API_KEY_FILE together")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SAGE_LLM_API_KEY_FILE") || !strings.Contains(msg, "only one") {
		t.Fatalf("error %q does not name the conflict", msg)
	}
	if strings.Contains(msg, "env-value-xyz") || strings.Contains(msg, "file-value-xyz") {
		t.Fatalf("error leaks a secret value: %q", msg)
	}
}

func TestReadSecretFile_TrimsExactlyOneTrailingNewline(t *testing.T) {
	cases := map[string]string{
		"key":         "key",
		"key\n":       "key",
		"key\r\n":     "key",
		"key\n\n":     "key\n",
		"key\r\n\r\n": "key\r\n",
		" key \n":     " key ",
		"a\nb\n":      "a\nb",
	}
	for body, want := range cases {
		got, err := readSecretFile("SAGE_X_FILE", writeSecretFile(t, body, 0o600))
		if err != nil {
			t.Fatalf("readSecretFile(%q): %v", body, err)
		}
		if got != want {
			t.Errorf("readSecretFile(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestReadSecretFile_Errors(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxSecretFileBytes+1)
	cases := map[string]struct {
		path string
		want string
	}{
		"missing":      {filepath.Join(dir, "nope"), "SAGE_X_FILE"},
		"directory":    {dir, "SAGE_X_FILE"},
		"empty":        {writeSecretFile(t, "", 0o600), "empty"},
		"only newline": {writeSecretFile(t, "\n", 0o600), "empty"},
		"too large":    {writeSecretFile(t, big, 0o600), "larger than"},
	}
	for name, tc := range cases {
		_, err := readSecretFile("SAGE_X_FILE", tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestReadSecretFile_ExactlyAtSizeLimitIsAccepted(t *testing.T) {
	body := strings.Repeat("y", maxSecretFileBytes)
	got, err := readSecretFile("SAGE_X_FILE", writeSecretFile(t, body, 0o600))
	if err != nil || len(got) != maxSecretFileBytes {
		t.Fatalf("len = %d, err = %v", len(got), err)
	}
}

func TestSecretFile_MissingFileFailsLoadNamingVariableAndPath(t *testing.T) {
	chdirTemp(t)
	clearSecretEnv(t)
	path := filepath.Join(t.TempDir(), "absent-key")
	t.Setenv("SAGE_ENCRYPTION_KEY_FILE", path)
	_, err := Load(nil)
	if err == nil || !strings.Contains(err.Error(), "SAGE_ENCRYPTION_KEY_FILE") ||
		!strings.Contains(err.Error(), "absent-key") {
		t.Fatalf("err = %v", err)
	}
}

type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "secret" }
func (f fakeFileInfo) Size() int64        { return 1 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func TestSecretFileTooOpen_PerPlatform(t *testing.T) {
	cases := []struct {
		goos string
		mode fs.FileMode
		want bool
	}{
		{"linux", 0o600, false},
		{"linux", 0o400, false},
		{"linux", 0o640, false}, // group-readable: the k8s fsGroup default
		{"linux", 0o644, true},
		{"linux", 0o604, true},
		{"linux", 0o602, true},
		{"darwin", 0o644, true},
		{"windows", 0o666, false}, // Windows reports 0666 for every writable file
	}
	for _, tc := range cases {
		if got := secretFileTooOpen(fakeFileInfo{tc.mode}, tc.goos); got != tc.want {
			t.Errorf("%s %o: tooOpen = %v, want %v", tc.goos, tc.mode, got, tc.want)
		}
	}
}

func TestLookupSecretEnv_WarnsOnWorldReadableFile(t *testing.T) {
	out := captureConfigWarnings(t)
	t.Setenv("SAGE_LLM_API_KEY", "")
	path := writeSecretFile(t, "k", 0o600)
	t.Setenv("SAGE_LLM_API_KEY_FILE", path)
	stat := func(string) (fs.FileInfo, error) { return fakeFileInfo{0o644}, nil }
	got, err := lookupSecretEnvWith("SAGE_LLM_API_KEY", stat, "linux")
	if err != nil || got != "k" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if !strings.Contains(out.String(), "SAGE_LLM_API_KEY_FILE") ||
		!strings.Contains(out.String(), "chmod 600") {
		t.Fatalf("warning = %q", out.String())
	}
	out.Reset()
	stat = func(string) (fs.FileInfo, error) { return fakeFileInfo{0o600}, nil }
	if _, err := lookupSecretEnvWith("SAGE_LLM_API_KEY", stat, "linux"); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("0600 file warned: %q", out.String())
	}
}

func TestLookupSecretEnv_NeitherSetIsEmpty(t *testing.T) {
	t.Setenv("SAGE_LLM_API_KEY", "")
	t.Setenv("SAGE_LLM_API_KEY_FILE", "")
	got, err := LookupSecretEnv("SAGE_LLM_API_KEY")
	if err != nil || got != "" {
		t.Fatalf("got %q, err %v", got, err)
	}
	t.Setenv("SAGE_LLM_API_KEY", "plain")
	if got, err := LookupSecretEnv("SAGE_LLM_API_KEY"); err != nil || got != "plain" {
		t.Fatalf("env only: got %q, err %v", got, err)
	}
}

func TestSecretFile_CLIFlagStillWins(t *testing.T) {
	chdirTemp(t)
	clearSecretEnv(t)
	t.Setenv("SAGE_DATABASE_URL_FILE",
		writeSecretFile(t, "postgres://file@h/db\n", 0o600))
	cfg, err := Load([]string{"--pg-url", "postgres://flag@h/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Postgres.DatabaseURL != "postgres://flag@h/db" {
		t.Fatalf("DatabaseURL = %q, want the flag", cfg.Postgres.DatabaseURL)
	}
}

func TestSecretFile_FileOverridesYAML(t *testing.T) {
	chdirTemp(t)
	clearSecretEnv(t)
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  api_key: from-yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAGE_LLM_API_KEY_FILE", writeSecretFile(t, "from-file", 0o600))
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.APIKey != "from-file" {
		t.Fatalf("APIKey = %q, want the file value over YAML", cfg.LLM.APIKey)
	}
}

func TestSecretFile_BracedYAMLReferenceReadsFile(t *testing.T) {
	chdirTemp(t)
	out := captureConfigWarnings(t)
	t.Setenv("SAGE_TEST_FLEET_PW", "")
	t.Setenv("SAGE_TEST_FLEET_PW_FILE", writeSecretFile(t, "s3cr$t-from-file\n", 0o600))
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "llm:\n  api_key: ${SAGE_TEST_FLEET_PW}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.APIKey != "s3cr$t-from-file" {
		t.Fatalf("APIKey = %q", cfg.LLM.APIKey)
	}
	if strings.Contains(out.String(), "SAGE_TEST_FLEET_PW") {
		t.Fatalf("a file-backed reference warned as unset: %q", out.String())
	}
}

func TestSecretFile_BracedYAMLReferenceWithBothSetFails(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_TEST_FLEET_PW", "env")
	t.Setenv("SAGE_TEST_FLEET_PW_FILE", writeSecretFile(t, "file", 0o600))
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  api_key: ${SAGE_TEST_FLEET_PW}\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "SAGE_TEST_FLEET_PW_FILE") {
		t.Fatalf("err = %v", err)
	}
}

// Hot reload re-reads the file: a rotated secret reaches the candidate.
func TestSecretFile_HotReloadSeesRotatedFile(t *testing.T) {
	chdirTemp(t)
	secret := writeSecretFile(t, "first", 0o600)
	t.Setenv("SAGE_TEST_ROTATE", "")
	t.Setenv("SAGE_TEST_ROTATE_FILE", secret)
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  api_key: ${SAGE_TEST_ROTATE}\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil || cfg.LLM.APIKey != "first" {
		t.Fatalf("first load: %q, %v", cfg.LLM.APIKey, err)
	}
	if err := os.WriteFile(secret, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := Clone(cfg)
	if err := loadYAML(path, candidate); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if candidate.LLM.APIKey != "second" {
		t.Fatalf("reloaded APIKey = %q, want the rotated value", candidate.LLM.APIKey)
	}
}

// No concurrent-access test: secret resolution reads the environment and
// files and keeps no state of its own.

func TestTLSPair_OneWithoutTheOtherFailsLoad(t *testing.T) {
	for _, set := range []string{"SAGE_TLS_CERT", "SAGE_TLS_KEY"} {
		chdirTemp(t)
		t.Setenv("SAGE_TLS_CERT", "")
		t.Setenv("SAGE_TLS_KEY", "")
		t.Setenv(set, "/etc/tls/x.pem")
		_, err := Load(nil)
		if err == nil || !strings.Contains(err.Error(), "SAGE_TLS_CERT") ||
			!strings.Contains(err.Error(), "SAGE_TLS_KEY") {
			t.Errorf("only %s set: err = %v", set, err)
		}
	}
}

func TestTLSPair_BothOrNeitherLoads(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_TLS_CERT", "")
	t.Setenv("SAGE_TLS_KEY", "")
	cfg, err := Load(nil)
	if err != nil || cfg.TLSEnabled() {
		t.Fatalf("neither: enabled=%v err=%v", cfg != nil && cfg.TLSEnabled(), err)
	}
	t.Setenv("SAGE_TLS_CERT", "/etc/tls/tls.crt")
	t.Setenv("SAGE_TLS_KEY", "/etc/tls/tls.key")
	cfg, err = Load(nil)
	if err != nil || !cfg.TLSEnabled() || cfg.TLSCert != "/etc/tls/tls.crt" ||
		cfg.TLSKey != "/etc/tls/tls.key" {
		t.Fatalf("both: cfg=%+v err=%v", cfg, err)
	}
}
