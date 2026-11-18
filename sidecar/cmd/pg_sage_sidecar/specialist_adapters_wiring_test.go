package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// mcp-02: the incident.io and Azure Monitor adapters are off until their
// secret is configured, and their routes are parsed at startup.

func TestSpecialistHandlerOptions_AlertAdaptersOffByDefault(t *testing.T) {
	opts, err := specialistHandlerOptions(config.DefaultConfig().Specialist)
	if err != nil || opts.IncidentIO != nil || opts.AzureMonitor != nil ||
		opts.PagerDuty != nil || opts.Webhook != nil {
		t.Fatalf("default adapters %+v %v", opts, err)
	}
}

func TestSpecialistHandlerOptions_AlertAdaptersFromConfig(t *testing.T) {
	sc := config.DefaultConfig().Specialist
	sc.IncidentIO.SigningSecret, sc.IncidentIO.DatabaseField = "whsec_c2VjcmV0", "DB"
	sc.IncidentIO.Routes = []string{"Orders=orders:lock_blocking"}
	sc.AzureMonitor.SharedSecret = "k"
	sc.AzureMonitor.Resources = []string{"/Subscriptions/S/servers/Orders-PG=orders"}
	opts, err := specialistHandlerOptions(sc)
	if err != nil || opts.IncidentIO == nil || opts.AzureMonitor == nil {
		t.Fatalf("adapters %+v %v", opts, err)
	}
	if opts.IncidentIO.Secret != "whsec_c2VjcmV0" || opts.IncidentIO.Field != "DB" ||
		opts.IncidentIO.Routes["orders"] != (specialist.PagerDutyRoute{Database: "orders",
			Family: "lock_blocking"}) {
		t.Fatalf("incident.io %+v", opts.IncidentIO)
	}
	if opts.AzureMonitor.Secret != "k" ||
		opts.AzureMonitor.Resources["/subscriptions/s/servers/orders-pg"].Database != "orders" {
		t.Fatalf("azure %+v", opts.AzureMonitor)
	}
}

func TestSpecialistHandlerOptions_InvalidAlertRouteFailsStartup(t *testing.T) {
	sc := config.DefaultConfig().Specialist
	sc.AzureMonitor.SharedSecret = "k"
	sc.AzureMonitor.Resources = []string{"/subscriptions/s=orders:operator"}
	if _, err := specialistHandlerOptions(sc); err == nil ||
		!strings.Contains(err.Error(), "specialist.azure_monitor.resources") {
		t.Fatalf("an invalid route must fail startup naming the key: %v", err)
	}
}
