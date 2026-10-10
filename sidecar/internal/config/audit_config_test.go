package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Defaults hold without any config file: verification daily, pgaudit
// correlation on, SIEM export off (no sinks), OAuth off with task_id.
func TestAuditDefaults(t *testing.T) {
	cfg := DefaultConfig()
	a := cfg.Audit
	if a.VerifyIntervalHours != 24 || !a.PGAudit.Correlate || len(a.SIEM.Sinks) != 0 ||
		a.SIEM.BatchSize != 200 || a.SIEM.IntervalSeconds != 10 ||
		a.SIEM.MaxBackoffSeconds != 300 || a.Evidence.SigningKeyEnv != "" {
		t.Fatalf("audit defaults = %+v", a)
	}
	o := cfg.MCP.OAuth
	if o.Enabled || o.TaskClaim != "task_id" || o.Resource != "" || len(o.Issuers) != 0 {
		t.Fatalf("mcp.oauth defaults = %+v", o)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
}

// Every bound is enforced, with the key named.
func TestAuditValidationBounds(t *testing.T) {
	cases := map[string]func(*AuditConfig){
		"audit.verify_interval_hours":    func(a *AuditConfig) { a.VerifyIntervalHours = -1 },
		"audit.siem.batch_size":          func(a *AuditConfig) { a.SIEM.BatchSize = 0 },
		"audit.siem.interval_seconds":    func(a *AuditConfig) { a.SIEM.IntervalSeconds = 3601 },
		"audit.siem.max_backoff_seconds": func(a *AuditConfig) { a.SIEM.MaxBackoffSeconds = 0 },
		"audit.evidence.signing_key_env": func(a *AuditConfig) {
			a.Evidence.SigningKeyEnv = "not a name"
		},
	}
	for key, mutate := range cases {
		a := defaultAuditConfig()
		mutate(&a)
		if err := a.validate(); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s: err = %v", key, err)
		}
	}
	a := defaultAuditConfig()
	a.VerifyIntervalHours, a.SIEM.BatchSize = 0, 1000
	if err := a.validate(); err != nil {
		t.Fatalf("boundary values refused: %v", err)
	}
}

// Sinks: each type needs its destination, names are unique slugs.
func TestAuditSinkValidation(t *testing.T) {
	good := []AuditSIEMSink{
		{Name: "splunk", Type: "http", URL: "https://hec.example.com/services/collector"},
		{Name: "rsyslog", Type: "syslog", Address: "10.0.0.5:6514", Network: "tls"},
		{Name: "udp-syslog", Type: "syslog", Address: "10.0.0.5:514", Network: "udp"},
		{Name: "otel", Type: "otlp", URL: "http://collector:4318", TokenEnv: "OTEL_TOKEN"},
	}
	a := defaultAuditConfig()
	a.SIEM.Sinks = good
	if err := a.validate(); err != nil {
		t.Fatalf("good sinks refused: %v", err)
	}
	bad := []AuditSIEMSink{
		{Name: "Bad Name", Type: "http", URL: "https://x"},
		{Name: "a", Type: "kafka", URL: "https://x"},
		{Name: "a", Type: "http", URL: "ftp://x"},
		{Name: "a", Type: "otlp", URL: ""},
		{Name: "a", Type: "syslog", Address: "no-port"},
		{Name: "a", Type: "syslog", Address: "h:514", Network: "sctp"},
		{Name: "a", Type: "http", URL: "https://x", TokenEnv: "lower"},
		{Name: "a", Type: "http", URL: "https://x", TimeoutSeconds: 301},
	}
	for i, s := range bad {
		a := defaultAuditConfig()
		a.SIEM.Sinks = []AuditSIEMSink{s}
		if err := a.validate(); err == nil {
			t.Fatalf("bad sink %d accepted: %+v", i, s)
		}
	}
	a.SIEM.Sinks = []AuditSIEMSink{good[0], good[0]}
	if err := a.validate(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate sink names: err = %v", err)
	}
}

// OAuth needs the http transport, a resource and an issuer once enabled.
func TestMCPOAuthValidation(t *testing.T) {
	ok := MCPOAuthConfig{Enabled: true, Resource: "https://sage.example.com/api/v1/mcp",
		Issuers: []MCPOAuthIssuer{{Issuer: "https://idp.example.com"}}}
	if err := ok.validate("http"); err != nil {
		t.Fatalf("valid oauth refused: %v", err)
	}
	cases := map[string]struct {
		cfg       MCPOAuthConfig
		transport string
	}{
		"transport": {ok, "stdio"},
		"resource":  {MCPOAuthConfig{Enabled: true, Issuers: ok.Issuers}, "http"},
		"issuers":   {MCPOAuthConfig{Enabled: true, Resource: ok.Resource}, "http"},
		"issuer": {MCPOAuthConfig{Enabled: true, Resource: ok.Resource,
			Issuers: []MCPOAuthIssuer{{JWKSURI: "https://k"}}}, "http"},
	}
	for want, c := range cases {
		if err := c.cfg.validate(c.transport); err == nil || !strings.Contains(err.Error(),
			want) {
			t.Fatalf("%s: err = %v", want, err)
		}
	}
	if err := (MCPOAuthConfig{}).validate("stdio"); err != nil {
		t.Fatalf("disabled oauth must not be checked: %v", err)
	}
}

// The YAML keys load into the structs.
func TestAuditYAMLLoads(t *testing.T) {
	cfg := DefaultConfig()
	raw := `
mcp:
  transport: http
  oauth:
    enabled: true
    resource: https://sage.example.com/api/v1/mcp
    issuers:
      - issuer: https://idp.example.com
        signing_algs: [ES256]
audit:
  verify_interval_hours: 6
  siem:
    batch_size: 50
    sinks:
      - {name: soc, type: syslog, address: "siem:6514", network: tls}
  evidence:
    signing_key_env: SAGE_EVIDENCE_KEY
  pgaudit:
    correlate: false
`
	path := filepath.Join(t.TempDir(), "sage.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.MCP.OAuth.Enabled || cfg.MCP.OAuth.Issuers[0].SigningAlgs[0] != "ES256" ||
		cfg.Audit.VerifyIntervalHours != 6 || cfg.Audit.SIEM.BatchSize != 50 ||
		cfg.Audit.SIEM.Sinks[0].Network != "tls" || cfg.Audit.PGAudit.Correlate ||
		cfg.Audit.Evidence.SigningKeyEnv != "SAGE_EVIDENCE_KEY" {
		t.Fatalf("loaded = %+v / %+v", cfg.MCP.OAuth, cfg.Audit)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("loaded config invalid: %v", err)
	}
}

// Clone detaches the E2 slices: editing a clone's issuers or sinks leaves
// the original alone.
func TestCloneDetachesAuditAndOAuth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MCP.OAuth.Issuers = []MCPOAuthIssuer{{Issuer: "https://a",
		SigningAlgs: []string{"RS256"}}}
	cfg.Audit.SIEM.Sinks = []AuditSIEMSink{{Name: "s", Chains: []string{"action_log"}}}
	cp := Clone(cfg)
	cp.MCP.OAuth.Issuers[0].Issuer = "https://b"
	cp.MCP.OAuth.Issuers[0].SigningAlgs[0] = "ES256"
	cp.Audit.SIEM.Sinks[0].Chains[0] = "auth_audit"
	if cfg.MCP.OAuth.Issuers[0].Issuer != "https://a" ||
		cfg.MCP.OAuth.Issuers[0].SigningAlgs[0] != "RS256" ||
		cfg.Audit.SIEM.Sinks[0].Chains[0] != "action_log" {
		t.Fatalf("clone aliases the original: %+v %+v", cfg.MCP.OAuth, cfg.Audit.SIEM)
	}
}
