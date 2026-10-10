package decommission

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"gopkg.in/yaml.v3"
)

func yamlNode(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
	}
	return doc.Content[0]
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func isolateLoad(t *testing.T) {
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

const minimalConfig = "mode: standalone\npostgres:\n  host: db.example\n  database: app\n"

// G0-07: with legacy keys present and live provisioning off, startup warns
// once and ignores them.
func TestLegacySection_WarnsOnceAndIsIgnored(t *testing.T) {
	resetLegacyWarning()
	t.Cleanup(resetLegacyWarning)
	node := yamlNode(t, "live_provisioning_enabled: false\nreconcile_interval_seconds: 30\n"+
		"providers:\n  aws_rds:\n    enabled: true\n")
	warnings, err := checkLegacySection(node)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ignored") ||
		!strings.Contains(warnings[0], "§12") {
		t.Fatalf("warnings = %v, want one naming §12", warnings)
	}
	again, err := checkLegacySection(node)
	if err != nil || len(again) != 0 {
		t.Fatalf("second check = %v, %v; the warning must appear once", again, err)
	}
}

// G0-07: with live provisioning on, startup refuses and names §12.
func TestLegacySection_LiveProvisioningRefusesStartup(t *testing.T) {
	resetLegacyWarning()
	t.Cleanup(resetLegacyWarning)
	for _, src := range []string{"live_provisioning_enabled: true\n",
		"live_provisioning_enabled: yes\nallow_public_ip: true\n"} {
		_, err := checkLegacySection(yamlNode(t, src))
		if !errors.Is(err, ErrLiveProvisioningEnabled) {
			t.Fatalf("%q: err = %v, want ErrLiveProvisioningEnabled", src, err)
		}
		if !strings.Contains(err.Error(), "§12") {
			t.Fatalf("refusal %q does not name §12", err)
		}
	}
}

func TestLegacySection_MalformedOrEmptySections(t *testing.T) {
	resetLegacyWarning()
	t.Cleanup(resetLegacyWarning)
	if _, err := checkLegacySection(yamlNode(t, "live_provisioning_enabled: maybe\n")); err == nil ||
		!strings.Contains(err.Error(), "live_provisioning_enabled") {
		t.Fatalf("an unreadable live flag must refuse, got %v", err)
	}
	for _, src := range []string{"", "true\n", "[]\n", "{}\n"} {
		resetLegacyWarning()
		warnings, err := checkLegacySection(yamlNode(t, src))
		if err != nil || len(warnings) != 1 {
			t.Fatalf("%q: warnings=%v err=%v, want one warning", src, warnings, err)
		}
	}
	if _, err := checkLegacySection(nil); err != nil {
		t.Fatalf("nil node: %v", err)
	}
}

func TestAckSection_ShapeIsChecked(t *testing.T) {
	for src, ok := range map[string]bool{
		"acknowledged_resources: [provider_resource:dep-1]\nexported: true\n": true,
		"acknowledged_resources: []\nexported: false\n":                       true,
		"acknowledged_resources: provider_resource:dep-1\n":                   false,
		"exported: sometimes\n":                                               false,
		"acknowledged: [x]\n":                                                 false,
		"[x]\n":                                                               false,
	} {
		_, err := checkAckSection(yamlNode(t, src))
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", src, err, ok)
		}
	}
}

func TestConfigLoad_IgnoresLegacySectionsEndToEnd(t *testing.T) {
	isolateLoad(t)
	resetLegacyWarning()
	t.Cleanup(resetLegacyWarning)
	path := writeConfig(t, minimalConfig+"agentdb:\n  reconcile_interval_seconds: 30\n"+
		"agentdb_decommission:\n  acknowledged_resources: [a]\n  exported: true\n"+
		"collector:\n  interval_seconds: 23\n")
	cfg, err := config.Load([]string{"-config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collector.IntervalSeconds != 23 || cfg.ConfigPath != path {
		t.Fatalf("the rest of the config did not load: %+v", cfg.Collector)
	}
}

func TestConfigLoad_RefusesLiveProvisioningEndToEnd(t *testing.T) {
	isolateLoad(t)
	path := writeConfig(t, minimalConfig+"agentdb:\n  live_provisioning_enabled: true\n")
	_, err := config.Load([]string{"-config", path})
	if !errors.Is(err, ErrLiveProvisioningEnabled) {
		t.Fatalf("Load error = %v, want the §12 refusal", err)
	}
}

func TestReadConfigAck(t *testing.T) {
	path := writeConfig(t, minimalConfig+"agentdb_decommission:\n"+
		"  acknowledged_resources: [provider_resource:dep-1, local_schema:dep-2]\n"+
		"  exported: true\n")
	ack, err := readConfigAck(path)
	if err != nil || ack == nil {
		t.Fatalf("readConfigAck = %+v, %v", ack, err)
	}
	if !ack.Exported || strings.Join(ack.AcknowledgedResources, ",") !=
		"provider_resource:dep-1,local_schema:dep-2" {
		t.Fatalf("ack = %+v", ack)
	}
	for name, body := range map[string]string{"absent": minimalConfig, "no path": ""} {
		p := ""
		if name == "absent" {
			p = writeConfig(t, body)
		}
		got, err := readConfigAck(p)
		if err != nil || got != nil {
			t.Fatalf("%s: readConfigAck = %+v, %v; want nil, nil", name, got, err)
		}
	}
	if _, err := readConfigAck(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("a missing config file must be an error")
	}
}
