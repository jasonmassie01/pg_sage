package config

import (
	"strings"
	"testing"
)

// specialist.incidentio and specialist.azure_monitor (mcp-02): the two
// inbound alert adapters are off until configured, and routes without
// their secret, or malformed routes, fail at load naming the key.

func TestSpecialistAlertAdapters_OffByDefault(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	io, az := cfg.Specialist.IncidentIO, cfg.Specialist.AzureMonitor
	if io.SigningSecret != "" || len(io.Routes) != 0 || io.DatabaseField != "Database" ||
		az.SharedSecret != "" || len(az.Resources) != 0 {
		t.Fatalf("adapter defaults io=%+v azure=%+v", io, az)
	}
}

func TestSpecialistAlertAdapters_FromYAML(t *testing.T) {
	cfg, err := loadRCAYAML(t, "specialist:\n  incidentio:\n    signing_secret: whsec_c2VjcmV0\n"+
		"    database_field: Postgres DB\n    routes: [\"orders=orders:lock_blocking\"]\n"+
		"  azure_monitor:\n    shared_secret: k\n"+
		"    resources: [\"/subscriptions/s/flexibleServers/orders-pg=orders\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	io, az := cfg.Specialist.IncidentIO, cfg.Specialist.AzureMonitor
	if io.SigningSecret != "whsec_c2VjcmV0" || io.DatabaseField != "Postgres DB" ||
		len(io.Routes) != 1 || az.SharedSecret != "k" || len(az.Resources) != 1 ||
		cfg.Specialist.WritesPerMinute != 30 {
		t.Fatalf("io=%+v azure=%+v", io, az)
	}
}

func TestSpecialistAlertAdapters_InvalidConfigNamesTheKey(t *testing.T) {
	cases := map[string]struct{ yaml, key string }{
		"io routes without secret": {"specialist:\n  incidentio:\n" +
			"    routes: [\"orders=orders\"]\n", "specialist.incidentio.signing_secret"},
		"io malformed route": {"specialist:\n  incidentio:\n    signing_secret: s\n" +
			"    routes: [\"orders\"]\n", "specialist.incidentio.routes[0]"},
		"io empty field": {"specialist:\n  incidentio:\n    signing_secret: s\n" +
			"    database_field: \" \"\n", "specialist.incidentio.database_field"},
		"azure resources without secret": {"specialist:\n  azure_monitor:\n" +
			"    resources: [\"/subscriptions/s=orders\"]\n",
			"specialist.azure_monitor.shared_secret"},
		"azure malformed resource": {"specialist:\n  azure_monitor:\n" +
			"    shared_secret: k\n    resources: [\"=orders\"]\n",
			"specialist.azure_monitor.resources[0]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadRCAYAML(t, c.yaml)
			if err == nil || !strings.Contains(err.Error(), c.key) {
				t.Fatalf("want an error naming %s, got %v", c.key, err)
			}
		})
	}
}

// A secret alone (no routes yet) is valid: the adapter answers every
// alert unknown_database until a route is added.
func TestSpecialistAlertAdapters_SecretWithoutRoutesIsValid(t *testing.T) {
	cfg, err := loadRCAYAML(t, "specialist:\n  incidentio:\n    signing_secret: s\n"+
		"  azure_monitor:\n    shared_secret: k\n")
	if err != nil || cfg.Specialist.IncidentIO.SigningSecret != "s" ||
		cfg.Specialist.AzureMonitor.SharedSecret != "k" {
		t.Fatalf("secret only: %+v %v", cfg.Specialist, err)
	}
}
