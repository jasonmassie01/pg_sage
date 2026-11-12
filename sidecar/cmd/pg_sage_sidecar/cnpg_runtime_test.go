package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// market-12: a databases[].cnpg block makes the provider cnpg (a refinement
// of self-managed, never an override of a detected cloud). Without one,
// CNPG evidence on the server opens finding cnpg_target_unconfigured.

// No concurrent access tests: these helpers are pure functions of their
// inputs; the probe is one bounded read-only query.

var cnpgBlock = config.CNPGConfig{Cluster: "pg-main", Namespace: "db"}

func TestCNPGProviderFromConfig(t *testing.T) {
	for _, tc := range []struct {
		detected string
		block    config.CNPGConfig
		want     string
		warns    bool
	}{
		{providerSelfManaged, cnpgBlock, "cnpg", false},
		{providerUnknown, cnpgBlock, "cnpg", false},
		{"rds", cnpgBlock, "rds", true},
		{"cloud-sql", cnpgBlock, "cloud-sql", true},
		{providerSelfManaged, config.CNPGConfig{}, providerSelfManaged, false},
		{providerUnknown, config.CNPGConfig{}, providerUnknown, false},
	} {
		got, warning := cnpgProvider(tc.detected, tc.block)
		if got != tc.want || (warning != "") != tc.warns {
			t.Errorf("%s with %+v = %q (warning %q), want %q", tc.detected, tc.block, got,
				warning, tc.want)
		}
		if tc.warns && !strings.Contains(warning, "db/pg-main") {
			t.Errorf("warning %q does not name the Cluster", warning)
		}
	}
}

func TestCNPGHint(t *testing.T) {
	manager := "/controller/manager wal-archive --log-destination " +
		"/controller/log/postgres.json %p"
	for _, tc := range []struct {
		name     string
		settings map[string]string
		want     string
	}{
		{"instance manager archive", map[string]string{"archive_command": manager},
			"instance manager"},
		{"pg17 alter system off", map[string]string{"allow_alter_system": "off",
			"archive_command": ""}, "allow_alter_system"},
		{"pg16 falls back to archive_command", map[string]string{
			"archive_command": manager}, "instance manager"},
		{"pg16 plain server", map[string]string{"archive_command": "cp %p /a/%f"}, ""},
		{"pg17 plain server", map[string]string{"allow_alter_system": "on",
			"archive_command": "(disabled)"}, ""},
		{"nothing read", map[string]string{}, ""},
		{"nil", nil, ""},
	} {
		got, ok := cnpgHint(tc.settings)
		if ok != (tc.want != "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: hint %q %v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestCNPGHintFinding(t *testing.T) {
	f := cnpgHintFinding("archive_command runs the CloudNativePG instance manager")
	if categoryCNPGHint != "cnpg_target_unconfigured" || f.Category != categoryCNPGHint ||
		f.Severity != "warning" || f.RecommendedSQL != "" || f.ObjectIdentifier == "" ||
		!strings.Contains(f.Recommendation, "cnpg:") ||
		!strings.Contains(f.Recommendation, "instance manager") ||
		f.Detail["evidence"] != "archive_command runs the CloudNativePG instance manager" {
		t.Fatalf("finding = %+v", f)
	}
}

func TestCNPGShareKeyIsPerCluster(t *testing.T) {
	a := cnpgShareKey(cnpgBlock)
	if a != "cnpg|cluster=db/pg-main" || cnpgShareKey(cnpgBlock) != a {
		t.Fatalf("share key = %q", a)
	}
	if cnpgShareKey(config.CNPGConfig{Cluster: "pg-main", Namespace: "other"}) == a {
		t.Fatal("another namespace is another Cluster")
	}
}

func TestCNPGResolverOutsideAPod(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	r, err := cnpgResolver(cnpgBlock)
	if err == nil || r != nil || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Fatalf("resolver %v err %v", r, err)
	}
}

// Integration: the probe reads the hint settings on real PostgreSQL. A
// plain test server is no CNPG Cluster.
func TestCNPGHintSettingsOnLivePostgres(t *testing.T) {
	pool := autonomyPool(t)
	settings, err := readCNPGHintSettings(context.Background(), pool)
	if err != nil {
		t.Fatalf("readCNPGHintSettings: %v", err)
	}
	if _, ok := settings["archive_command"]; !ok {
		t.Fatalf("settings = %v, want archive_command", settings)
	}
	var num int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["allow_alter_system"]; ok != (num >= 170000) {
		t.Fatalf("allow_alter_system present %v on %d", ok, num)
	}
	if hint, ok := cnpgHint(settings); ok {
		t.Fatalf("a plain server is hinted as CNPG: %q", hint)
	}
}
