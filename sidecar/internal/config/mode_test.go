package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config-10: an explicit standalone mode with a meta database used to pass
// validation, then main ran the meta-db fleet while the metrics reported
// standalone. The combination is now refused at Load.

const standaloneMetaMessage = "mode standalone cannot be combined with " +
	"meta_db; drop mode (meta is inferred) or set mode: meta"

const metaYAMLTail = `meta_db: "postgres://sage@meta.internal/sage_meta"
encryption_key: "config-10-test-passphrase-0123456789"
`

func TestLoad_StandaloneWithMetaDBYAML_Fails(t *testing.T) {
	cfg, err := loadYAMLResult(t, "mode: standalone\n"+metaYAMLTail)
	if err == nil {
		t.Fatalf("Load accepted mode standalone with meta_db: mode=%q", cfg.Mode)
	}
	if !strings.Contains(err.Error(), standaloneMetaMessage) {
		t.Fatalf("error = %q, want it to contain %q", err, standaloneMetaMessage)
	}
}

func TestLoad_StandaloneYAMLWithMetaDBFlag_Fails(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: standalone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load([]string{
		"-config", path,
		"-meta-db", "postgres://sage@meta.internal/sage_meta",
		"-encryption-key", "config-10-test-passphrase-0123456789",
	})
	if err == nil || !strings.Contains(err.Error(), standaloneMetaMessage) {
		t.Fatalf("error = %v, want %q", err, standaloneMetaMessage)
	}
}

func TestLoad_StandaloneEnvWithMetaDBEnv_Fails(t *testing.T) {
	chdirTemp(t) // clears SAGE_MODE, so the variables are set after it
	t.Setenv("SAGE_MODE", "standalone")
	t.Setenv("SAGE_META_DB", "postgres://sage@meta.internal/sage_meta")
	t.Setenv("SAGE_ENCRYPTION_KEY", "config-10-test-passphrase-0123456789")
	_, err := Load(nil)
	if err == nil || !strings.Contains(err.Error(), standaloneMetaMessage) {
		t.Fatalf("error = %v, want %q", err, standaloneMetaMessage)
	}
}

func TestLoad_MetaDBWithoutModeInfersMeta(t *testing.T) {
	cfg, err := loadYAMLResult(t, metaYAMLTail)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeMeta {
		t.Fatalf("mode = %q, want %q (inferred from meta_db)", cfg.Mode, ModeMeta)
	}
}

func TestLoad_ExplicitMetaWithMetaDB_Succeeds(t *testing.T) {
	cfg, err := loadYAMLResult(t, "mode: meta\n"+metaYAMLTail)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mode != ModeMeta || cfg.MetaDB == "" {
		t.Fatalf("mode=%q meta_db=%q, want meta with a meta_db", cfg.Mode, cfg.MetaDB)
	}
}

func TestValidateMode_Table(t *testing.T) {
	cases := []struct {
		name, mode, metaDB, wantErr string
	}{
		{"standalone alone", "standalone", "", ""},
		{"fleet alone", "fleet", "", ""},
		{"meta with meta_db", ModeMeta, "postgres://m", ""},
		{"standalone with meta_db", "standalone", "postgres://m", standaloneMetaMessage},
		{"meta without meta_db", ModeMeta, "", "requires --meta-db"},
		{"empty mode", "", "", "invalid mode"},
		{"unknown mode", "Standalone", "", "invalid mode"},
		{"extension mode", "extension", "", "removed with the C extension"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMode(&Config{Mode: tc.mode, MetaDB: tc.metaDB})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMode: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateMode error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// No concurrent access test: validateMode and Load's mode checks read only
// the config they are given and share no state.
