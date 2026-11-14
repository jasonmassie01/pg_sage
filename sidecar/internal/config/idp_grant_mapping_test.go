package config

import (
	"strings"
	"testing"
)

// apiauth-08d: oauth.database_grant_mapping maps IdP groups to per-database
// grants. It is checked at load: an unknown role, a malformed entry or a
// repeated group and database stops startup and names the entry. Empty
// (the default) keeps today's behaviour.

// No concurrent access tests: validation is a pure function of one Config.

func idpGrantConfig(entries ...OAuthDatabaseGrant) *Config {
	cfg := DefaultConfig()
	cfg.OAuth.DatabaseGrantMapping = entries
	return cfg
}

func TestIdPGrantMapping_DefaultIsEmpty(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.OAuth.DatabaseGrantMapping != nil {
		t.Fatalf("DatabaseGrantMapping default = %+v, want nil", cfg.OAuth.DatabaseGrantMapping)
	}
	if err := cfg.validateAuthz(); err != nil {
		t.Fatalf("default config: %v", err)
	}
}

func TestIdPGrantMapping_ValidEntriesLoad(t *testing.T) {
	cfg := idpGrantConfig(
		OAuthDatabaseGrant{Group: "team-billing", Database: "billing", Role: "operator"},
		OAuthDatabaseGrant{Group: "team-billing", Database: "orders", Role: "viewer"},
		OAuthDatabaseGrant{Group: "team-search", Database: "billing", Role: "viewer"},
		// Group names are case-sensitive, so these are two different groups.
		OAuthDatabaseGrant{Group: "Team-Billing", Database: "billing", Role: "viewer"},
	)
	if err := cfg.validateAuthz(); err != nil {
		t.Fatalf("validateAuthz: %v, want nil", err)
	}
}

func TestIdPGrantMapping_InvalidEntriesAreLoadErrors(t *testing.T) {
	ok := OAuthDatabaseGrant{Group: "team-billing", Database: "billing", Role: "operator"}
	cases := map[string]struct {
		bad  OAuthDatabaseGrant
		want []string
	}{
		"admin role": {OAuthDatabaseGrant{Group: "team-ops", Database: "orders",
			Role: "admin"}, []string{"database_grant_mapping[1]", `"admin"`, "team-ops",
			"viewer or operator"}},
		"empty role": {OAuthDatabaseGrant{Group: "team-ops", Database: "orders"},
			[]string{"database_grant_mapping[1]", "role"}},
		"upper-case role": {OAuthDatabaseGrant{Group: "team-ops", Database: "orders",
			Role: "Operator"}, []string{"database_grant_mapping[1]", "role"}},
		"empty group": {OAuthDatabaseGrant{Group: " ", Database: "orders", Role: "viewer"},
			[]string{"database_grant_mapping[1]", "group"}},
		"empty database": {OAuthDatabaseGrant{Group: "team-ops", Role: "viewer"},
			[]string{"database_grant_mapping[1]", "database"}},
		"all database": {OAuthDatabaseGrant{Group: "team-ops", Database: "all",
			Role: "viewer"}, []string{"database_grant_mapping[1]", "database"}},
		"wildcard database": {OAuthDatabaseGrant{Group: "team-ops", Database: "*",
			Role: "viewer"}, []string{"database_grant_mapping[1]", "database"}},
		"duplicate group and database": {OAuthDatabaseGrant{Group: "team-billing",
			Database: "billing", Role: "viewer"}, []string{"database_grant_mapping[1]",
			"team-billing", "billing", "[0]"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := idpGrantConfig(ok, tc.bad).validateAuthz()
			if err == nil {
				t.Fatalf("validateAuthz accepted %+v", tc.bad)
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err, part)
				}
			}
		})
	}
}

func TestIdPGrantMapping_FromYAML(t *testing.T) {
	dir := chdirAuthzTemp(t)
	writeAuthzFile(t, dir, "config.yaml", "oauth:\n  database_grant_mapping:\n"+
		"    - {group: team-billing, database: billing, role: operator}\n"+
		"    - {group: team-search, database: search, role: viewer}\n")
	cfg, err := Load([]string{"--mode=standalone"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []OAuthDatabaseGrant{
		{Group: "team-billing", Database: "billing", Role: "operator"},
		{Group: "team-search", Database: "search", Role: "viewer"},
	}
	got := cfg.OAuth.DatabaseGrantMapping
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("DatabaseGrantMapping = %+v, want %+v", got, want)
	}
}

func TestIdPGrantMapping_AdminRoleInYAMLFailsStartup(t *testing.T) {
	dir := chdirAuthzTemp(t)
	writeAuthzFile(t, dir, "config.yaml", "oauth:\n  database_grant_mapping:\n"+
		"    - {group: team-billing, database: billing, role: admin}\n")
	_, err := Load([]string{"--mode=standalone"})
	if err == nil || !strings.Contains(err.Error(), "database_grant_mapping[0]") ||
		!strings.Contains(err.Error(), "admin") {
		t.Fatalf("Load with an admin grant: err = %v, want an error naming entry [0]", err)
	}
}

func TestIdPGrantMapping_CloneDoesNotShare(t *testing.T) {
	cfg := idpGrantConfig(OAuthDatabaseGrant{Group: "a", Database: "b", Role: "viewer"})
	cp := Clone(cfg)
	cp.OAuth.DatabaseGrantMapping[0].Role = "operator"
	if cfg.OAuth.DatabaseGrantMapping[0].Role != "viewer" {
		t.Fatal("Clone shares the DatabaseGrantMapping backing array")
	}
}

func TestIdPGrantMapping_IsRestartOnlyAndSafetyCritical(t *testing.T) {
	class, ok := KeyClassOf("oauth.database_grant_mapping")
	if !ok || class != KeySafetyCritical {
		t.Fatalf("oauth.database_grant_mapping class = %q (%v), want safety_critical",
			class, ok)
	}
	if !isRestartField("oauth.database_grant_mapping") {
		t.Fatal("oauth.database_grant_mapping must be restart-only")
	}
}
