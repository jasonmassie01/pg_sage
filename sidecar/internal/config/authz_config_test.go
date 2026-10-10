package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real bcrypt hash (cost 4) of the throwaway fixture string
// "fixture-break-glass-pw"; it guards nothing.
const fixtureBcryptHash = "$2a$04$C1ODnQFDul1rqzdJ52pN1uFXtmzyOArkugPCm1mrPJqtPc4WJgGey"

func chdirTemp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func writeSecretFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLookupSecretEnv_PlainEnv(t *testing.T) {
	t.Setenv("SAGE_TEST_SECRET", "from-env")
	t.Setenv("SAGE_TEST_SECRET_FILE", "")
	v, ok, err := LookupSecretEnv("SAGE_TEST_SECRET")
	if err != nil || !ok || v != "from-env" {
		t.Fatalf("LookupSecretEnv = (%q, %v, %v), want from-env", v, ok, err)
	}
}

func TestLookupSecretEnv_FileTrimsTrailingNewlines(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"lf": "from-file\n", "crlf": "from-file\r\n", "none": "from-file",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SAGE_TEST_SECRET", "")
			t.Setenv("SAGE_TEST_SECRET_FILE", writeSecretFile(t, dir, name, body))
			v, ok, err := LookupSecretEnv("SAGE_TEST_SECRET")
			if err != nil || !ok || v != "from-file" {
				t.Fatalf("LookupSecretEnv = (%q, %v, %v), want from-file", v, ok, err)
			}
		})
	}
}

func TestLookupSecretEnv_FileKeepsInnerWhitespace(t *testing.T) {
	path := writeSecretFile(t, t.TempDir(), "s", "pass phrase with spaces\n")
	t.Setenv("SAGE_TEST_SECRET", "")
	t.Setenv("SAGE_TEST_SECRET_FILE", path)
	v, _, err := LookupSecretEnv("SAGE_TEST_SECRET")
	if err != nil || v != "pass phrase with spaces" {
		t.Fatalf("LookupSecretEnv = (%q, %v)", v, err)
	}
}

func TestLookupSecretEnv_Unset(t *testing.T) {
	t.Setenv("SAGE_TEST_SECRET", "")
	t.Setenv("SAGE_TEST_SECRET_FILE", "")
	v, ok, err := LookupSecretEnv("SAGE_TEST_SECRET")
	if err != nil || ok || v != "" {
		t.Fatalf("LookupSecretEnv unset = (%q, %v, %v), want empty, false, nil", v, ok, err)
	}
}

func TestLookupSecretEnv_BothSetIsAnError(t *testing.T) {
	path := writeSecretFile(t, t.TempDir(), "s", "file")
	t.Setenv("SAGE_TEST_SECRET", "env")
	t.Setenv("SAGE_TEST_SECRET_FILE", path)
	v, _, err := LookupSecretEnv("SAGE_TEST_SECRET")
	if err == nil || v != "" {
		t.Fatalf("LookupSecretEnv both set = (%q, %v), want an error", v, err)
	}
	if !strings.Contains(err.Error(), "SAGE_TEST_SECRET_FILE") {
		t.Fatalf("error %q does not name the _FILE variable", err)
	}
	if strings.Contains(err.Error(), "env") && strings.Contains(err.Error(), "file\"") {
		t.Fatalf("error %q leaks a secret value", err)
	}
}

func TestLookupSecretEnv_FileErrors(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"missing file": filepath.Join(dir, "absent"),
		"empty file":   writeSecretFile(t, dir, "empty", ""),
		"only newline": writeSecretFile(t, dir, "nl", "\n"),
		"directory":    dir,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SAGE_TEST_SECRET", "")
			t.Setenv("SAGE_TEST_SECRET_FILE", path)
			v, ok, err := LookupSecretEnv("SAGE_TEST_SECRET")
			if err == nil || ok || v != "" {
				t.Fatalf("LookupSecretEnv = (%q, %v, %v), want an error", v, ok, err)
			}
			if !strings.Contains(err.Error(), "SAGE_TEST_SECRET_FILE") {
				t.Fatalf("error %q does not name the variable", err)
			}
		})
	}
}

func TestLookupSecretEnv_OversizedFileRejected(t *testing.T) {
	path := writeSecretFile(t, t.TempDir(), "big", strings.Repeat("x", maxSecretFileBytes+1))
	t.Setenv("SAGE_TEST_SECRET", "")
	t.Setenv("SAGE_TEST_SECRET_FILE", path)
	if _, _, err := LookupSecretEnv("SAGE_TEST_SECRET"); err == nil {
		t.Fatal("oversized secret file accepted")
	}
}

func TestLoad_EncryptionKeysFromFiles(t *testing.T) {
	dir := chdirTemp(t)
	t.Setenv("SAGE_ENCRYPTION_KEY", "")
	t.Setenv("SAGE_ENCRYPTION_KEY_FILE", writeSecretFile(t, dir, "k", "new-passphrase\n"))
	t.Setenv("SAGE_ENCRYPTION_KEY_PREVIOUS", "")
	t.Setenv("SAGE_ENCRYPTION_KEY_PREVIOUS_FILE",
		writeSecretFile(t, dir, "p", "old-passphrase\n"))
	cfg, err := Load([]string{"--mode=standalone"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EncryptionKey != "new-passphrase" {
		t.Fatalf("EncryptionKey = %q, want the file value", cfg.EncryptionKey)
	}
	if cfg.EncryptionKeyPrevious != "old-passphrase" {
		t.Fatalf("EncryptionKeyPrevious = %q, want the file value", cfg.EncryptionKeyPrevious)
	}
}

func TestLoad_EncryptionKeyFileErrorFailsStartup(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_ENCRYPTION_KEY", "")
	t.Setenv("SAGE_ENCRYPTION_KEY_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := Load([]string{"--mode=standalone"}); err == nil ||
		!strings.Contains(err.Error(), "SAGE_ENCRYPTION_KEY_FILE") {
		t.Fatalf("Load with unreadable key file: err = %v, want one naming the variable", err)
	}
}

func TestLoad_PreviousKeyWithoutActiveKeyRejected(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_ENCRYPTION_KEY", "")
	t.Setenv("SAGE_ENCRYPTION_KEY_FILE", "")
	t.Setenv("SAGE_ENCRYPTION_KEY_PREVIOUS", "old-passphrase")
	_, err := Load([]string{"--mode=standalone"})
	if err == nil || !strings.Contains(err.Error(), "encryption_key_previous") {
		t.Fatalf("Load: err = %v, want encryption_key_previous error", err)
	}
}

func TestLoad_BreakGlassHashFromFile(t *testing.T) {
	dir := chdirTemp(t)
	yaml := "oauth:\n  break_glass:\n    enabled: true\n"
	writeSecretFile(t, dir, "config.yaml", yaml)
	t.Setenv("SAGE_BREAK_GLASS_PASSWORD_HASH", "")
	t.Setenv("SAGE_BREAK_GLASS_PASSWORD_HASH_FILE",
		writeSecretFile(t, dir, "h", fixtureBcryptHash+"\n"))
	cfg, err := Load([]string{"--mode=standalone"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OAuth.BreakGlass.Enabled || cfg.OAuth.BreakGlass.PasswordHash != fixtureBcryptHash {
		t.Fatalf("break glass = %+v, want enabled with the file hash", cfg.OAuth.BreakGlass)
	}
}

func TestAuthzDefaultsWithoutConfigFile(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.OAuth.GroupsClaim != "groups" {
		t.Errorf("GroupsClaim default = %q, want groups", cfg.OAuth.GroupsClaim)
	}
	if cfg.OAuth.UnmappedUsers != UnmappedUsersDeny {
		t.Errorf("UnmappedUsers default = %q, want %q", cfg.OAuth.UnmappedUsers, UnmappedUsersDeny)
	}
	if cfg.OAuth.BreakGlass.Enabled || cfg.OAuth.BreakGlass.PasswordHash != "" {
		t.Errorf("break glass must be off by default: %+v", cfg.OAuth.BreakGlass)
	}
	if len(cfg.OAuth.RoleMapping) != 0 {
		t.Errorf("RoleMapping default = %v, want empty", cfg.OAuth.RoleMapping)
	}
	if cfg.EncryptionKeyPrevious != "" {
		t.Errorf("EncryptionKeyPrevious default = %q", cfg.EncryptionKeyPrevious)
	}
}

func TestValidateAuthz(t *testing.T) {
	valid := func() *Config {
		cfg := DefaultConfig()
		cfg.OAuth.RoleMapping = []OAuthRoleMapping{
			{Group: "pgsage-admins", Role: "admin"}, {Group: "dba", Role: "operator"},
		}
		return cfg
	}
	cases := map[string]struct {
		mutate func(*Config)
		want   string // "" = valid
	}{
		"valid mapping":        {func(*Config) {}, ""},
		"default role mode":    {func(c *Config) { c.OAuth.UnmappedUsers = "default_role" }, ""},
		"break glass enabled":  {func(c *Config) { enableBreakGlass(c, fixtureBcryptHash) }, ""},
		"unknown mapped role":  {func(c *Config) { c.OAuth.RoleMapping[0].Role = "root" }, "role"},
		"empty mapped role":    {func(c *Config) { c.OAuth.RoleMapping[0].Role = "" }, "role"},
		"empty group":          {func(c *Config) { c.OAuth.RoleMapping[1].Group = " " }, "group"},
		"bad unmapped policy":  {func(c *Config) { c.OAuth.UnmappedUsers = "allow" }, "unmapped_users"},
		"empty unmapped":       {func(c *Config) { c.OAuth.UnmappedUsers = "" }, "unmapped_users"},
		"empty groups claim":   {func(c *Config) { c.OAuth.GroupsClaim = "" }, "groups_claim"},
		"break glass no hash":  {func(c *Config) { enableBreakGlass(c, "") }, "password_hash"},
		"break glass not hash": {func(c *Config) { enableBreakGlass(c, "plaintext-pw") }, "bcrypt"},
		"disabled with junk":   {func(c *Config) { c.OAuth.BreakGlass.PasswordHash = "junk" }, ""},
		"bad default role":     {func(c *Config) { c.OAuth.DefaultRole = "superuser" }, "default_role"},
		"previous without key": {func(c *Config) { c.EncryptionKeyPrevious = "old" }, "previous"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			err := cfg.validateAuthz()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateAuthz: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateAuthz = %v, want error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "plaintext-pw") {
				t.Fatalf("error %q echoes a secret", err)
			}
		})
	}
}

func enableBreakGlass(c *Config, hash string) {
	c.OAuth.BreakGlass.Enabled = true
	c.OAuth.BreakGlass.PasswordHash = hash
}

func TestLoad_RoleMappingFromYAML(t *testing.T) {
	dir := chdirTemp(t)
	yaml := "oauth:\n  groups_claim: roles\n  unmapped_users: default_role\n" +
		"  role_mapping:\n    - group: sre\n      role: operator\n" +
		"    - group: platform-admins\n      role: admin\n"
	writeSecretFile(t, dir, "config.yaml", yaml)
	cfg, err := Load([]string{"--mode=standalone"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []OAuthRoleMapping{{Group: "sre", Role: "operator"},
		{Group: "platform-admins", Role: "admin"}}
	if len(cfg.OAuth.RoleMapping) != 2 || cfg.OAuth.RoleMapping[0] != want[0] ||
		cfg.OAuth.RoleMapping[1] != want[1] {
		t.Fatalf("RoleMapping = %+v, want %+v", cfg.OAuth.RoleMapping, want)
	}
	if cfg.OAuth.GroupsClaim != "roles" || cfg.OAuth.UnmappedUsers != "default_role" {
		t.Fatalf("claim/policy = %q/%q", cfg.OAuth.GroupsClaim, cfg.OAuth.UnmappedUsers)
	}
}

func TestLoad_InvalidRoleMappingFailsStartup(t *testing.T) {
	dir := chdirTemp(t)
	writeSecretFile(t, dir, "config.yaml",
		"oauth:\n  role_mapping:\n    - group: sre\n      role: god\n")
	if _, err := Load([]string{"--mode=standalone"}); err == nil ||
		!strings.Contains(err.Error(), "role") {
		t.Fatalf("Load with invalid mapping: err = %v", err)
	}
}

func TestCloneCopiesRoleMapping(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OAuth.RoleMapping = []OAuthRoleMapping{{Group: "a", Role: "admin"}}
	cp := Clone(cfg)
	cp.OAuth.RoleMapping[0].Role = "viewer"
	if cfg.OAuth.RoleMapping[0].Role != "admin" {
		t.Fatal("Clone shares the RoleMapping backing array")
	}
}
