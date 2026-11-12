package config

import (
	"strings"
	"testing"
)

// No concurrent access tests: the discovery config is a value validated
// once at load; nothing shares it mutably.

func validClusterSource() DiscoverySourceConfig {
	return DiscoverySourceConfig{Name: "prod-cluster", Type: "cluster",
		Owner: "team-db", Member: "prod-main", ReuseCredentials: true}
}

func TestFleetDiscoveryDefaults_WithoutAnyConfig(t *testing.T) {
	d := DefaultConfig().Fleet.Discovery
	if d.Enabled {
		t.Fatal("fleet.discovery.enabled must default to false")
	}
	if d.MaxEnrolled != 10 {
		t.Fatalf("max_enrolled default = %d, want 10", d.MaxEnrolled)
	}
	if d.TTLHours != 72 {
		t.Fatalf("ttl_hours default = %d, want 72", d.TTLHours)
	}
	if len(d.Sources) != 0 {
		t.Fatalf("no sources by default, got %d", len(d.Sources))
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("default discovery config invalid: %v", err)
	}
}

func TestFleetDiscoveryValidate_SourceWithoutOwnerFails(t *testing.T) {
	d := DefaultConfig().Fleet.Discovery
	src := validClusterSource()
	src.Owner = "  "
	d.Sources = []DiscoverySourceConfig{src}
	err := d.Validate()
	if err == nil || !strings.Contains(err.Error(), "owner") ||
		!strings.Contains(err.Error(), "prod-cluster") {
		t.Fatalf("want an owner error naming the source, got %v", err)
	}
}

func TestFleetDiscoveryValidate_ConfigValidateRejectsMissingOwner(t *testing.T) {
	cfg := DefaultConfig()
	src := validClusterSource()
	src.Owner = ""
	cfg.Fleet.Discovery.Sources = []DiscoverySourceConfig{src}
	if err := cfg.validate(); err == nil ||
		!strings.Contains(err.Error(), "fleet.discovery") {
		t.Fatalf("Config.validate must reject the source, got %v", err)
	}
}

func TestFleetDiscoveryValidate_InvalidSources(t *testing.T) {
	cases := []struct {
		name string
		edit func(*DiscoverySourceConfig)
		want string
	}{
		{"empty name", func(s *DiscoverySourceConfig) { s.Name = "" }, "name"},
		{"bad name", func(s *DiscoverySourceConfig) { s.Name = "Prod Cluster!" }, "name"},
		{"long name", func(s *DiscoverySourceConfig) {
			s.Name = strings.Repeat("a", 33)
		}, "name"},
		{"unknown type", func(s *DiscoverySourceConfig) { s.Type = "gcp" }, "type"},
		{"cluster without member", func(s *DiscoverySourceConfig) { s.Member = "" }, "member"},
		{"lakebase without project", func(s *DiscoverySourceConfig) {
			*s = DiscoverySourceConfig{Name: "lb", Type: "lakebase", Owner: "o"}
		}, "project"},
		{"lakebase reusing credentials", func(s *DiscoverySourceConfig) {
			*s = DiscoverySourceConfig{Name: "lb", Type: "lakebase", Owner: "o",
				Project: "p1", ReuseCredentials: true}
		}, "reuse_credentials"},
		{"rds without region", func(s *DiscoverySourceConfig) {
			*s = DiscoverySourceConfig{Name: "aws", Type: "rds", Owner: "o",
				TagKey: "team"}
		}, "region"},
		{"rds without tag", func(s *DiscoverySourceConfig) {
			*s = DiscoverySourceConfig{Name: "aws", Type: "rds", Owner: "o",
				Region: "us-east-1"}
		}, "tag_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DefaultConfig().Fleet.Discovery
			src := validClusterSource()
			tc.edit(&src)
			d.Sources = []DiscoverySourceConfig{src}
			err := d.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFleetDiscoveryValidate_DuplicateSourceNames(t *testing.T) {
	d := DefaultConfig().Fleet.Discovery
	d.Sources = []DiscoverySourceConfig{validClusterSource(), validClusterSource()}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate-name error, got %v", err)
	}
}

func TestFleetDiscoveryValidate_Boundaries(t *testing.T) {
	cases := []struct {
		maxEnrolled, ttl int
		ok               bool
	}{
		{0, 72, true}, // 0 = candidates only
		{50, 72, true},
		{51, 72, false},
		{-1, 72, false},
		{10, 1, true},
		{10, 720, true},
		{10, 0, false},
		{10, 721, false},
	}
	for _, tc := range cases {
		d := DefaultConfig().Fleet.Discovery
		d.MaxEnrolled, d.TTLHours = tc.maxEnrolled, tc.ttl
		err := d.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("max_enrolled=%d ttl_hours=%d: err=%v, want ok=%v",
				tc.maxEnrolled, tc.ttl, err, tc.ok)
		}
	}
}

func TestFleetDiscoveryValidate_AllSourceTypesAccepted(t *testing.T) {
	d := DefaultConfig().Fleet.Discovery
	d.Enabled = true
	d.Sources = []DiscoverySourceConfig{
		validClusterSource(),
		{Name: "lakebase-dev", Type: "lakebase", Owner: "data", Project: "p1"},
		{Name: "aws", Type: "rds", Owner: "platform", Region: "us-east-1",
			TagKey: "pg_sage", TagValue: "discover"},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("valid sources rejected: %v", err)
	}
	if got := d.TTL().Hours(); got != 72 {
		t.Fatalf("TTL() = %v hours, want 72", got)
	}
}
