package config

import (
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// The E1 secrets (the previous encryption key and the break-glass hash)
// follow the one NAME / NAME_FILE rule set of secret_files.go.

func TestSecretEnvNames_IncludeTheAuthzSecrets(t *testing.T) {
	names := SecretEnvNames()
	for _, want := range []string{"SAGE_ENCRYPTION_KEY", "SAGE_ENCRYPTION_KEY_PREVIOUS",
		"SAGE_BREAK_GLASS_PASSWORD_HASH"} {
		if !slices.Contains(names, want) {
			t.Errorf("SecretEnvNames() lacks %s: %v", want, names)
		}
	}
}

func TestLoad_AuthzSecretBothSetIsAnError(t *testing.T) {
	for _, name := range []string{"SAGE_ENCRYPTION_KEY_PREVIOUS",
		"SAGE_BREAK_GLASS_PASSWORD_HASH"} {
		t.Run(name, func(t *testing.T) {
			dir := chdirAuthzTemp(t)
			t.Setenv("SAGE_ENCRYPTION_KEY", "active-passphrase")
			t.Setenv(name, "from-env")
			t.Setenv(name+"_FILE", writeAuthzFile(t, dir, "s", "from-file\n"))
			_, err := Load([]string{"--mode=standalone"})
			if err == nil || !strings.Contains(err.Error(), name+"_FILE") ||
				!strings.Contains(err.Error(), "both set") {
				t.Fatalf("Load: err = %v, want both-set error naming %s_FILE", err, name)
			}
			if strings.Contains(err.Error(), "from-env") ||
				strings.Contains(err.Error(), "from-file") {
				t.Fatalf("error %q leaks a secret value", err)
			}
		})
	}
}

func TestLoad_AuthzSecretsFromPlainEnv(t *testing.T) {
	chdirAuthzTemp(t)
	t.Setenv("SAGE_ENCRYPTION_KEY", "active-passphrase")
	t.Setenv("SAGE_ENCRYPTION_KEY_FILE", "")
	t.Setenv("SAGE_ENCRYPTION_KEY_PREVIOUS", "old-passphrase")
	t.Setenv("SAGE_ENCRYPTION_KEY_PREVIOUS_FILE", "")
	t.Setenv("SAGE_BREAK_GLASS_PASSWORD_HASH", fixtureBcryptHash)
	t.Setenv("SAGE_BREAK_GLASS_PASSWORD_HASH_FILE", "")
	cfg, err := Load([]string{"--mode=standalone"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EncryptionKeyPrevious != "old-passphrase" {
		t.Errorf("EncryptionKeyPrevious = %q, want the env value", cfg.EncryptionKeyPrevious)
	}
	if cfg.OAuth.BreakGlass.PasswordHash != fixtureBcryptHash {
		t.Errorf("break-glass hash = %q, want the env value",
			cfg.OAuth.BreakGlass.PasswordHash)
	}
}

func TestLookupSecretEnv_AuthzSecretFileOpenToAllWarns(t *testing.T) {
	for _, name := range []string{"SAGE_ENCRYPTION_KEY_PREVIOUS",
		"SAGE_BREAK_GLASS_PASSWORD_HASH"} {
		t.Run(name, func(t *testing.T) {
			out := captureConfigWarnings(t)
			t.Setenv(name, "")
			t.Setenv(name+"_FILE", writeAuthzFile(t, t.TempDir(), "s", "v\n"))
			open := func(string) (fs.FileInfo, error) { return fakeFileInfo{0o644}, nil }
			got, err := lookupSecretEnvWith(name, open, "linux")
			if err != nil || got != "v" {
				t.Fatalf("lookup = (%q, %v), want v", got, err)
			}
			if !strings.Contains(out.String(), name+"_FILE") ||
				!strings.Contains(out.String(), "chmod 600") {
				t.Fatalf("warning %q does not name %s_FILE", out.String(), name)
			}
		})
	}
}
