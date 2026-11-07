package config

import (
	"os"
	"path/filepath"
	"testing"
)

// No concurrent access tests: APIListenAddrFromFile reads one file and keeps
// no state.

func writeListenYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestAPIListenAddrFromFileReadsOnlyTheAPIAddress(t *testing.T) {
	// A config whose database settings would fail Load must still answer.
	path := writeListenYAML(t, "mode: fleet\ndatabases: []\napi:\n"+
		"  listen_addr: \" 0.0.0.0:9443 \"\n")
	got, err := APIListenAddrFromFile(path)
	if err != nil {
		t.Fatalf("APIListenAddrFromFile: %v", err)
	}
	if got != "0.0.0.0:9443" {
		t.Fatalf("listen addr = %q, want 0.0.0.0:9443", got)
	}
}

func TestAPIListenAddrFromFileExpandsBracedEnv(t *testing.T) {
	t.Setenv("SAGE_TEST_LISTEN", "127.0.0.1:7443")
	path := writeListenYAML(t, "api:\n  listen_addr: ${SAGE_TEST_LISTEN}\n")
	got, err := APIListenAddrFromFile(path)
	if err != nil {
		t.Fatalf("APIListenAddrFromFile: %v", err)
	}
	if got != "127.0.0.1:7443" {
		t.Fatalf("listen addr = %q, want the expanded 127.0.0.1:7443", got)
	}
}

func TestAPIListenAddrFromFileUnsetIsEmpty(t *testing.T) {
	for name, body := range map[string]string{
		"no api section": "mode: standalone\n",
		"empty value":    "api:\n  listen_addr: \"\"\n",
		"empty file":     "",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := APIListenAddrFromFile(writeListenYAML(t, body))
			if err != nil {
				t.Fatalf("APIListenAddrFromFile: %v", err)
			}
			if got != "" {
				t.Fatalf("listen addr = %q, want empty", got)
			}
		})
	}
}

func TestAPIListenAddrFromFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	if _, err := APIListenAddrFromFile(missing); err == nil {
		t.Fatal("missing file: want an error")
	}
	bad := writeListenYAML(t, "api: [unterminated\n")
	if _, err := APIListenAddrFromFile(bad); err == nil {
		t.Fatal("malformed YAML: want an error")
	}
	wrongType := writeListenYAML(t, "api:\n  listen_addr: [a, b]\n")
	if _, err := APIListenAddrFromFile(wrongType); err == nil {
		t.Fatal("list-valued listen_addr: want an error")
	}
}
