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

// Each chains entry must be a chain the migration installs; a typo is
// refused naming the five valid chains, not exported as nothing.
func TestAuditSinkChainsMustBeKnown(t *testing.T) {
	base := AuditSIEMSink{Name: "soc", Type: "http", URL: "https://siem.example.com/x"}
	bad := base
	bad.Chains = []string{"action-log"}
	err := bad.validate()
	if err == nil {
		t.Fatalf("chains [action-log] accepted")
	}
	for _, want := range []string{"audit.siem.sinks[soc].chains", `"action-log"`,
		"action_log", "auth_audit", "config_audit", "guard_query_audit", "pgaudit_events"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
	mixed := base
	mixed.Chains = []string{"auth_audit", ""}
	if err := mixed.validate(); err == nil {
		t.Fatalf("an empty chain name was accepted")
	}
	all := base
	all.Chains = []string{"action_log", "auth_audit", "config_audit", "guard_query_audit",
		"pgaudit_events"}
	if err := all.validate(); err != nil {
		t.Fatalf("the five built-in chains were refused: %v", err)
	}
	if err := base.validate(); err != nil {
		t.Fatalf("empty chains (all chains) refused: %v", err)
	}
}

// TLS files: a client certificate needs its key and the reverse, and TLS
// files on a transport without TLS are refused rather than ignored.
func TestAuditSinkTLSFileRules(t *testing.T) {
	https := AuditSIEMSink{Name: "soc", Type: "http", URL: "https://siem.example.com/x"}
	tlsSyslog := AuditSIEMSink{Name: "rsys", Type: "syslog", Address: "siem:6514",
		Network: "tls"}
	with := func(s AuditSIEMSink, ca, cert, key string) AuditSIEMSink {
		s.CAFile, s.ClientCertFile, s.ClientKeyFile = ca, cert, key
		return s
	}
	otlp := https
	otlp.Type = "otlp"
	good := []AuditSIEMSink{
		https, with(https, "/etc/sage/ca.pem", "", ""),
		with(https, "", "/etc/sage/c.pem", "/etc/sage/c.key"),
		with(otlp, "/etc/sage/ca.pem", "/etc/sage/c.pem", "/etc/sage/c.key"),
		with(tlsSyslog, "/etc/sage/ca.pem", "/etc/sage/c.pem", "/etc/sage/c.key"),
	}
	for i, s := range good {
		if err := s.validate(); err != nil {
			t.Fatalf("good sink %d refused: %v", i, err)
		}
	}
	plainHTTP := https
	plainHTTP.URL = "http://siem.example.com/x"
	tcp, udp, bare := tlsSyslog, tlsSyslog, tlsSyslog
	tcp.Network, udp.Network, bare.Network = "tcp", "udp", ""
	bad := map[string]AuditSIEMSink{
		"client_key_file":        with(https, "", "/etc/sage/c.pem", ""),
		"client_cert_file":       with(https, "", "", "/etc/sage/c.key"),
		"udp":                    with(udp, "/etc/sage/ca.pem", "", ""),
		"tcp":                    with(tcp, "", "/etc/sage/c.pem", "/etc/sage/c.key"),
		"tcp ":                   with(bare, "/etc/sage/ca.pem", "", ""),
		"http://":                with(plainHTTP, "/etc/sage/ca.pem", "", ""),
		"audit.siem.sinks[rsys]": with(udp, "", "", "/etc/sage/c.key"),
	}
	for want, s := range bad {
		err := s.validate()
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Fatalf("%s: err = %v", want, err)
		}
	}
}

// A token over plain http still validates in this release but carries the
// token_over_http warning, unless allow_insecure_http says it is meant.
func TestAuditSinkTokenOverHTTPWarning(t *testing.T) {
	s := AuditSIEMSink{Name: "soc", Type: "http", URL: "http://siem.internal/x",
		TokenEnv: "SIEM_TOKEN"}
	if err := s.validate(); err != nil {
		t.Fatalf("http+token must validate (warn only) in this release: %v", err)
	}
	if got := s.Warnings(); len(got) != 1 || got[0] != SIEMWarningTokenOverHTTP ||
		SIEMWarningTokenOverHTTP != "token_over_http" {
		t.Fatalf("warnings = %v", got)
	}
	o := s
	o.Type, o.URL = "otlp", "HTTP://collector:4318"
	if got := o.Warnings(); len(got) != 1 || got[0] != "token_over_http" {
		t.Fatalf("otlp over http with a token: warnings = %v", got)
	}
	quiet := map[string]AuditSIEMSink{}
	allowed := s
	allowed.AllowInsecureHTTP = true
	quiet["allow_insecure_http"] = allowed
	tls := s
	tls.URL = "https://siem.internal/x"
	quiet["https"] = tls
	noToken := s
	noToken.TokenEnv = ""
	quiet["no token"] = noToken
	quiet["syslog"] = AuditSIEMSink{Name: "rsys", Type: "syslog", Address: "h:514",
		Network: "tcp", TokenEnv: "SIEM_TOKEN"}
	for name, q := range quiet {
		if err := q.validate(); err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
		if got := q.Warnings(); len(got) != 0 {
			t.Fatalf("%s: warnings = %v", name, got)
		}
	}
}

// The TLS keys load from YAML.
func TestAuditYAMLLoadsSinkTLSKeys(t *testing.T) {
	cfg := DefaultConfig()
	raw := `
audit:
  siem:
    sinks:
      - name: soc
        type: http
        url: https://siem.example.com/x
        ca_file: /etc/sage/siem-ca.pem
        client_cert_file: /etc/sage/siem.crt
        client_key_file: /etc/sage/siem.key
        chains: [action_log, auth_audit]
      - {name: lab, type: http, url: "http://lab:8088", token_env: LAB_TOKEN,
         allow_insecure_http: true}
`
	path := filepath.Join(t.TempDir(), "sage.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Audit.SIEM.Sinks
	if len(s) != 2 || s[0].CAFile != "/etc/sage/siem-ca.pem" ||
		s[0].ClientCertFile != "/etc/sage/siem.crt" ||
		s[0].ClientKeyFile != "/etc/sage/siem.key" || s[0].AllowInsecureHTTP ||
		!s[1].AllowInsecureHTTP || len(s[1].Warnings()) != 0 {
		t.Fatalf("loaded sinks = %+v", s)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("loaded config invalid: %v", err)
	}
}

// Loading a config file with a misspelt chain fails, naming the right one
// (what pg_sage --config does at start).
func TestLoadRefusesMisspeltSinkChain(t *testing.T) {
	_, err := loadYAMLResult(t, standaloneIOYAML+`
audit:
  siem:
    sinks:
      - {name: soc, type: http, url: "https://siem.example.com/x", chains: [action-log]}
`)
	if err == nil || !strings.Contains(err.Error(), "action_log") ||
		!strings.Contains(err.Error(), "action-log") {
		t.Fatalf("load with chains [action-log]: err = %v", err)
	}
}

// No concurrency or Postgres tests here: validation is a pure function of
// one sink's fields and touches no shared state or database.
