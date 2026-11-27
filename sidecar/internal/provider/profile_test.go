package provider

import (
	"strings"
	"sync"
	"testing"
)

func TestLookupCanonicalNames(t *testing.T) {
	cases := map[string]struct {
		managed   bool
		mechanism ConfigMechanism
		logAccess string
	}{
		Postgres:    {false, ConfigAlterSystem, "local"},
		RDS:         {true, ConfigParameterGroup, "cloudwatch"},
		Aurora:      {true, ConfigClusterParameterGroup, "cloudwatch"},
		CloudSQL:    {true, ConfigCloudSQLFlags, "provider_logging"},
		AlloyDB:     {true, ConfigAlloyDBFlags, "cloud-logging"},
		Azure:       {true, ConfigServerParameters, "azure_monitor"},
		AzureCosmos: {true, ConfigGuidanceOnly, "azure_monitor"},
		Neon:        {true, ConfigTenant, "provider_console"},
		Supabase:    {true, ConfigTenant, "provider_console"},
		Lakebase:    {true, ConfigTenant, "unavailable"},
		CNPG:        {true, ConfigClusterSpec, "kubernetes"},
	}
	for name, want := range cases {
		p, ok := Lookup(name)
		if !ok || p.Name != name {
			t.Fatalf("Lookup(%q) = %q, %v", name, p.Name, ok)
		}
		if p.Managed != want.managed || p.ConfigMechanism != want.mechanism ||
			p.LogAccess != want.logAccess {
			t.Errorf("%s: managed %v mechanism %q log %q, want %+v", name, p.Managed,
				p.ConfigMechanism, p.LogAccess, want)
		}
	}
	if len(Profiles()) != len(cases) {
		t.Errorf("Profiles() has %d entries, test covers %d", len(Profiles()), len(cases))
	}
}

// Boundary: each alias resolves to exactly one profile.
func TestLookupAliasesResolveToOneProfile(t *testing.T) {
	cases := map[string]string{
		"cloudsql": CloudSQL, "gcp": CloudSQL, "cloud sql": CloudSQL,
		"gcp-cloud-sql": CloudSQL, "aws": RDS, "aws-rds": RDS,
		"aurora-postgresql": Aurora, "azure-flexible": Azure, "azure-single": Azure,
		"azure-postgres": Azure, "self-managed": Postgres, "postgresql": Postgres,
	}
	for alias, want := range cases {
		matches := 0
		for _, p := range Profiles() {
			for _, a := range append([]string{p.Name}, p.Aliases...) {
				if a == alias {
					matches++
				}
			}
		}
		if matches != 1 {
			t.Errorf("alias %q appears in %d profiles, want 1", alias, matches)
		}
		if p, ok := Lookup(alias); !ok || p.Name != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", alias, p.Name, ok, want)
		}
	}
}

func TestLookupInvalidAndCaseFolded(t *testing.T) {
	for _, name := range []string{"", " ", "unknown", "UNKNOWN", "cosmos", "azure cosmos",
		"rds;drop", "postgres\n"} {
		if name == "postgres\n" {
			continue // trailing whitespace is trimmed; covered below
		}
		if p, ok := Lookup(name); ok {
			t.Errorf("Lookup(%q) matched %q", name, p.Name)
		}
	}
	for in, want := range map[string]string{"AZURE-COSMOS ": AzureCosmos,
		" Cloud-SQL\t": CloudSQL, "postgres\n": Postgres, "AWS": RDS} {
		if p, ok := Lookup(in); !ok || p.Name != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", in, p.Name, ok, want)
		}
	}
	if Canonical("unknown") != "" || Canonical("") != "" || Canonical("GCP") != CloudSQL {
		t.Errorf("Canonical: unknown %q empty %q gcp %q", Canonical("unknown"),
			Canonical(""), Canonical("GCP"))
	}
}

func TestIsManaged(t *testing.T) {
	for name, want := range map[string]bool{"azure-cosmos": true, "lakebase": true,
		"aws": true, "self-managed": false, "postgres": false, "": false,
		"unknown": false, "bogus": false} {
		if got := IsManaged(name); got != want {
			t.Errorf("IsManaged(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestProfilesInvariants(t *testing.T) {
	for _, p := range Profiles() {
		if p.Name != strings.ToLower(strings.TrimSpace(p.Name)) || p.Name == Unknown {
			t.Errorf("profile name %q is not canonical", p.Name)
		}
		if p.Managed == (p.ConfigMechanism == ConfigAlterSystem) {
			t.Errorf("%s: managed %v with mechanism %q", p.Name, p.Managed, p.ConfigMechanism)
		}
		if p.Managed == (len(p.PreloadSteps) == 0) {
			t.Errorf("%s: managed %v with %d preload steps", p.Name, p.Managed,
				len(p.PreloadSteps))
		}
		if p.LogAccess == "" || p.LogAccess == "unknown" {
			t.Errorf("%s: log access %q", p.Name, p.LogAccess)
		}
	}
}

// A caller mutating a returned profile must not change the registry.
func TestLookupReturnsCopies(t *testing.T) {
	p, _ := Lookup(Azure)
	p.Aliases[0] = "mutated"
	p.PreloadSteps[0] = "mutated"
	again, _ := Lookup(Azure)
	if again.Aliases[0] == "mutated" || again.PreloadSteps[0] == "mutated" {
		t.Fatalf("registry was mutated through a returned profile: %+v", again)
	}
	all := Profiles()
	all[0].Name = "mutated"
	if Profiles()[0].Name == "mutated" {
		t.Fatal("Profiles() exposes the registry slice")
	}
}

func TestPreloadStepsExpand(t *testing.T) {
	steps := PreloadSteps(AzureCosmos, "pg_stat_statements")
	joined := strings.Join(steps, " ")
	for _, want := range []string{"PG_STAT_STATEMENTS", "az cosmosdb postgres configuration"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cosmos steps %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "flexible-server") || strings.Contains(joined, "{") {
		t.Errorf("cosmos steps %q", joined)
	}
	if got := PreloadSteps("aws", "auto_explain"); len(got) != 2 ||
		!strings.Contains(got[0], "DB parameter group, add auto_explain") {
		t.Errorf("aws steps %q", got)
	}
	got := PreloadSteps("supabase", "x")
	if len(got) != 1 || got[0] != "x is preloaded on supabase." {
		t.Errorf("supabase steps %q", got)
	}
	for _, name := range []string{"postgres", "self-managed", "", "unknown"} {
		if got := PreloadSteps(name, "x"); got != nil {
			t.Errorf("PreloadSteps(%q) = %q, want nil (self-managed steps)", name, got)
		}
	}
}

func TestLookupConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, ok := Lookup("AZURE-COSMOS")
			if !ok || p.Name != AzureCosmos {
				errs <- p.Name
			}
		}()
	}
	wg.Wait()
	close(errs)
	for name := range errs {
		t.Errorf("concurrent Lookup returned %q", name)
	}
}
