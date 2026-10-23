package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
)

// AuditConfig is tamper-evident audit export and evidence (E2, spec
// §6.17): the SIEM export of the hash-chained audit as OCSF, pgaudit
// correlation, evidence-pack signing and periodic chain verification.
type AuditConfig struct {
	SIEM                AuditSIEMConfig     `yaml:"siem"`
	Evidence            AuditEvidenceConfig `yaml:"evidence"`
	PGAudit             AuditPGAuditConfig  `yaml:"pgaudit"`
	VerifyIntervalHours int                 `yaml:"verify_interval_hours" doc:"Hours between verifications of the audit hash chains; a broken chain raises a critical finding. 0 disables. 0-168. Default: 24."`
}

// AuditSIEMConfig ships every audit chain link to SIEM sinks as OCSF.
type AuditSIEMConfig struct {
	Sinks             []AuditSIEMSink `yaml:"sinks" doc:"SIEM destinations: [{name, type: http|syslog|otlp, url, address, network, token_env, timeout_seconds, chains}]. Each gets every audit event at least once, in order."`
	BatchSize         int             `yaml:"batch_size" doc:"Events per delivery. 1-1000. Default: 200."`
	IntervalSeconds   int             `yaml:"interval_seconds" doc:"Seconds between export passes when caught up. 1-3600. Default: 10."`
	MaxBackoffSeconds int             `yaml:"max_backoff_seconds" doc:"Longest wait between retries of a failing sink; the cursor holds, so nothing is lost. 1-3600. Default: 300."`
}

// AuditSIEMSink is one SIEM destination.
type AuditSIEMSink struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	URL            string   `yaml:"url"`
	Address        string   `yaml:"address"`
	Network        string   `yaml:"network"`
	TokenEnv       string   `yaml:"token_env"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Chains         []string `yaml:"chains"`
}

// AuditEvidenceConfig configures evidence packs.
type AuditEvidenceConfig struct {
	SigningKeyEnv string `yaml:"signing_key_env" doc:"Environment variable (or its _FILE twin) holding an Ed25519 private key (PKCS#8 PEM) that signs evidence-pack manifests. Empty: packs are hashed (SHA256SUMS), not signed."`
}

// AuditPGAuditConfig configures pgaudit correlation.
type AuditPGAuditConfig struct {
	Correlate bool `yaml:"correlate" doc:"Where pgaudit is installed and the server log is readable, keep the pgaudit records of pg_sage actions and agent principals. Default: true."`
}

// Audit defaults.
const (
	DefaultAuditVerifyIntervalHours = 24
	DefaultSIEMBatchSize            = 200
	DefaultSIEMIntervalSeconds      = 10
	DefaultSIEMMaxBackoffSeconds    = 300
	DefaultSIEMTimeoutSeconds       = 10
)

func defaultAuditConfig() AuditConfig {
	return AuditConfig{VerifyIntervalHours: DefaultAuditVerifyIntervalHours,
		SIEM: AuditSIEMConfig{BatchSize: DefaultSIEMBatchSize,
			IntervalSeconds:   DefaultSIEMIntervalSeconds,
			MaxBackoffSeconds: DefaultSIEMMaxBackoffSeconds},
		PGAudit: AuditPGAuditConfig{Correlate: true}}
}

var (
	sinkName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	envName  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
)

func (a AuditConfig) validate() error {
	ints := []struct {
		key       string
		v, lo, hi int
	}{
		{"audit.verify_interval_hours", a.VerifyIntervalHours, 0, 168},
		{"audit.siem.batch_size", a.SIEM.BatchSize, 1, 1000},
		{"audit.siem.interval_seconds", a.SIEM.IntervalSeconds, 1, 3600},
		{"audit.siem.max_backoff_seconds", a.SIEM.MaxBackoffSeconds, 1, 3600},
	}
	for _, c := range ints {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("%s must be %d-%d, got %d", c.key, c.lo, c.hi, c.v)
		}
	}
	if a.Evidence.SigningKeyEnv != "" && !envName.MatchString(a.Evidence.SigningKeyEnv) {
		return fmt.Errorf("audit.evidence.signing_key_env %q is not an environment "+
			"variable name", a.Evidence.SigningKeyEnv)
	}
	seen := map[string]bool{}
	for _, s := range a.SIEM.Sinks {
		if seen[s.Name] {
			return fmt.Errorf("audit.siem.sinks: name %q is used twice", s.Name)
		}
		seen[s.Name] = true
		if err := s.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s AuditSIEMSink) validate() error {
	if !sinkName.MatchString(s.Name) {
		return fmt.Errorf("audit.siem.sinks: name %q must match %s", s.Name, sinkName)
	}
	if s.TimeoutSeconds < 0 || s.TimeoutSeconds > 300 {
		return fmt.Errorf("audit.siem.sinks[%s].timeout_seconds must be 0-300", s.Name)
	}
	if s.TokenEnv != "" && !envName.MatchString(s.TokenEnv) {
		return fmt.Errorf("audit.siem.sinks[%s].token_env %q is not an environment "+
			"variable name", s.Name, s.TokenEnv)
	}
	switch s.Type {
	case "http", "otlp":
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("audit.siem.sinks[%s].url %q must be an http(s) URL",
				s.Name, s.URL)
		}
	case "syslog":
		if _, _, err := net.SplitHostPort(s.Address); err != nil {
			return fmt.Errorf("audit.siem.sinks[%s].address %q must be host:port",
				s.Name, s.Address)
		}
		switch s.Network {
		case "", "tcp", "udp", "tls":
		default:
			return fmt.Errorf("audit.siem.sinks[%s].network must be tcp, udp or tls",
				s.Name)
		}
	default:
		return fmt.Errorf("audit.siem.sinks[%s].type must be http, syslog or otlp, "+
			"got %q", s.Name, s.Type)
	}
	return nil
}

// cloneAuditAndOAuth detaches the E2 slices of cp from cfg's.
func cloneAuditAndOAuth(cp *Config, cfg *Config) {
	cp.MCP.OAuth.Issuers = append([]MCPOAuthIssuer(nil), cfg.MCP.OAuth.Issuers...)
	for i := range cp.MCP.OAuth.Issuers {
		cp.MCP.OAuth.Issuers[i].SigningAlgs = append([]string(nil),
			cfg.MCP.OAuth.Issuers[i].SigningAlgs...)
	}
	cp.Audit.SIEM.Sinks = append([]AuditSIEMSink(nil), cfg.Audit.SIEM.Sinks...)
	for i := range cp.Audit.SIEM.Sinks {
		cp.Audit.SIEM.Sinks[i].Chains = append([]string(nil),
			cfg.Audit.SIEM.Sinks[i].Chains...)
	}
}
