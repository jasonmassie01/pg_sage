package config

import (
	"strings"
	"testing"
)

// No concurrent access tests: validation is a pure function of the config.

func TestAlloyDBResourceValidation(t *testing.T) {
	cases := []struct {
		name, provider string
		gcp            CloudResourceGCP
		want           string // "" = valid
	}{
		{"cluster and instance", "alloydb",
			CloudResourceGCP{Project: "shop-prod", Cluster: "orders", Instance: "p1"}, ""},
		{"no provider override", "", CloudResourceGCP{Cluster: "orders", Instance: "p1"}, ""},
		{"cluster without instance", "alloydb", CloudResourceGCP{Cluster: "orders"},
			"cluster and instance together"},
		{"cluster on cloud-sql", "cloud-sql",
			CloudResourceGCP{Cluster: "orders", Instance: "p1"}, "AlloyDB"},
		{"invalid cluster", "alloydb", CloudResourceGCP{Cluster: "Orders!", Instance: "p1"},
			`cluster "Orders!" is not a valid name`},
		{"cloud sql instance only", "cloud-sql", CloudResourceGCP{Instance: "main"}, ""},
		{"alloydb instance only", "alloydb", CloudResourceGCP{Instance: "main"},
			"cluster and instance together"},
	}
	for _, tc := range cases {
		err := validateCloudIdentity("postgres", tc.provider,
			CloudResourceConfig{GCP: tc.gcp})
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestCloudTelemetryAlloyDBIdentity(t *testing.T) {
	ok := CloudTelemetryConfig{AlloyDB: CloudTelemetryAlloyDB{Project: "shop-prod",
		Cluster: "orders", Instance: "p1"}}
	if err := ok.validateIdentity("standalone"); err != nil {
		t.Fatalf("valid standalone alloydb: %v", err)
	}
	if err := ok.validateIdentity("fleet"); err == nil ||
		!strings.Contains(err.Error(), "standalone only") {
		t.Fatalf("fleet alloydb names: err = %v, want standalone only", err)
	}
	half := CloudTelemetryConfig{AlloyDB: CloudTelemetryAlloyDB{Cluster: "orders"}}
	if err := half.validateIdentity("standalone"); err == nil ||
		!strings.Contains(err.Error(), "cluster and instance together") {
		t.Fatalf("half alloydb: err = %v", err)
	}
	bad := CloudTelemetryConfig{AlloyDB: CloudTelemetryAlloyDB{Cluster: "o",
		Instance: "UPPER"}}
	if err := bad.validateIdentity("standalone"); err == nil ||
		!strings.Contains(err.Error(), "alloydb.instance") {
		t.Fatalf("invalid alloydb instance: err = %v", err)
	}
	if (CloudTelemetryConfig{}).validateIdentity("fleet") != nil {
		t.Fatal("empty alloydb names must stay valid in fleet mode")
	}
}
