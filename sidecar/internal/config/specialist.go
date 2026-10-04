package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// SpecialistConfig configures the Postgres-specialist contract (roadmap
// phase 3): the versioned HTTP investigation API other agents (AWS DevOps
// Agent, PagerDuty, Datadog) call with MCP tokens. On by default: it is
// token-only, and agents can open and read investigations and request a
// remediation that pg_sage's gate decides, never approve one.
type SpecialistConfig struct {
	Enabled            bool                      `yaml:"enabled" doc:"Serve the Postgres-specialist contract under /api/v1/specialist (MCP tokens only). Default: true."`
	WritesPerMinute    int                       `yaml:"writes_per_minute" doc:"Opens and remediation requests one token may make per minute, 1-10000. Default: 30."`
	ReadsPerMinute     int                       `yaml:"reads_per_minute" doc:"Status, result and stream calls one token may make per minute, 1-10000. Default: 240."`
	MaxOpenPerIdentity int                       `yaml:"max_open_per_identity" doc:"Live investigations one token may have opened at once, 1-100. Attaching to an existing investigation never counts. Default: 3."`
	MaxOpenTotal       int                       `yaml:"max_open_total" doc:"Live investigations all tokens together may have opened at once, 1-100. Default: 10."`
	KeepIdentifiers    bool                      `yaml:"keep_identifiers" doc:"Keep schema, table and other identifiers in results. Secrets and PII are always removed; false replaces identifiers by keyed hashes like a replay export. Default: true."`
	PagerDuty          SpecialistPagerDutyConfig `yaml:"pagerduty"`
	Webhook            SpecialistWebhookConfig   `yaml:"webhook"`
}

// SpecialistPagerDutyConfig maps PagerDuty v3 incident webhooks onto the
// contract and posts results back as incident notes.
type SpecialistPagerDutyConfig struct {
	SigningSecret string   `yaml:"signing_secret" doc:"Secret of the PagerDuty webhook subscription; X-PagerDuty-Signature must match. Required when services are mapped." secret:"true"`
	Services      []string `yaml:"services" doc:"PagerDuty service id to database mappings, SERVICE_ID=database or SERVICE_ID=database:family. Unmapped services are ignored."`
	APIURL        string   `yaml:"api_url" doc:"PagerDuty REST API base URL for result notes (https). Empty sends no notes."`
	APIToken      string   `yaml:"api_token" doc:"PagerDuty REST API token used to post result notes." secret:"true"`
	FromEmail     string   `yaml:"from_email" doc:"PagerDuty user e-mail sent as the From header of result notes."`
}

// SpecialistWebhookConfig configures the generic signed webhook adapter.
type SpecialistWebhookConfig struct {
	SigningSecret             string `yaml:"signing_secret" doc:"HMAC secret of the generic webhook (X-Sage-Signature over timestamp.body) and of the result posts. Empty disables the adapter." secret:"true"`
	ResultURL                 string `yaml:"result_url" doc:"Where the signed result of a webhook-opened investigation is posted (https, or http to a loopback address). Empty posts nothing."`
	TimestampToleranceSeconds int    `yaml:"timestamp_tolerance_seconds" doc:"Maximum age (and clock skew) of a signed webhook request, 30-900 seconds. Default: 300."`
}

// Specialist defaults.
const (
	DefaultSpecialistWritesPerMinute    = 30
	DefaultSpecialistReadsPerMinute     = 240
	DefaultSpecialistMaxOpenPerIdentity = 3
	DefaultSpecialistMaxOpenTotal       = 10
	DefaultSpecialistWebhookTolerance   = 300
)

func defaultSpecialistConfig() SpecialistConfig {
	return SpecialistConfig{Enabled: true,
		WritesPerMinute:    DefaultSpecialistWritesPerMinute,
		ReadsPerMinute:     DefaultSpecialistReadsPerMinute,
		MaxOpenPerIdentity: DefaultSpecialistMaxOpenPerIdentity,
		MaxOpenTotal:       DefaultSpecialistMaxOpenTotal, KeepIdentifiers: true,
		Webhook: SpecialistWebhookConfig{
			TimestampToleranceSeconds: DefaultSpecialistWebhookTolerance}}
}

// Tolerance is the accepted age of a signed webhook request.
func (w SpecialistWebhookConfig) Tolerance() time.Duration {
	return time.Duration(w.TimestampToleranceSeconds) * time.Second
}

func (s SpecialistConfig) validate() error {
	for _, r := range []struct {
		key           string
		value, lo, hi int
	}{
		{"writes_per_minute", s.WritesPerMinute, 1, 10000},
		{"reads_per_minute", s.ReadsPerMinute, 1, 10000},
		{"max_open_per_identity", s.MaxOpenPerIdentity, 1, 100},
		{"max_open_total", s.MaxOpenTotal, 1, 100},
		{"webhook.timestamp_tolerance_seconds", s.Webhook.TimestampToleranceSeconds, 30, 900},
	} {
		if r.value < r.lo || r.value > r.hi {
			return fmt.Errorf("specialist.%s must be %d-%d, got %d", r.key, r.lo, r.hi,
				r.value)
		}
	}
	if s.MaxOpenPerIdentity > s.MaxOpenTotal {
		return fmt.Errorf("specialist.max_open_per_identity (%d) exceeds "+
			"specialist.max_open_total (%d)", s.MaxOpenPerIdentity, s.MaxOpenTotal)
	}
	if err := s.PagerDuty.validate(); err != nil {
		return err
	}
	return s.Webhook.validate()
}

func (p SpecialistPagerDutyConfig) validate() error {
	if len(p.Services) > 0 && p.SigningSecret == "" {
		return fmt.Errorf("specialist.pagerduty.services need " +
			"specialist.pagerduty.signing_secret")
	}
	for i, entry := range p.Services {
		service, database, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(service) == "" || strings.TrimSpace(database) == "" {
			return fmt.Errorf("specialist.pagerduty.services[%d] must be "+
				"SERVICE_ID=database[:family], got %q", i, entry)
		}
	}
	if p.APIURL == "" {
		return nil
	}
	if err := outboundURL("specialist.pagerduty.api_url", p.APIURL); err != nil {
		return err
	}
	if p.APIToken == "" || strings.TrimSpace(p.FromEmail) == "" {
		return fmt.Errorf("specialist.pagerduty.api_url needs " +
			"specialist.pagerduty.api_token and specialist.pagerduty.from_email")
	}
	return nil
}

func (w SpecialistWebhookConfig) validate() error {
	if w.ResultURL == "" {
		return nil
	}
	if w.SigningSecret == "" {
		return fmt.Errorf("specialist.webhook.result_url needs " +
			"specialist.webhook.signing_secret (results are signed)")
	}
	return outboundURL("specialist.webhook.result_url", w.ResultURL)
}

// outboundURL accepts an https URL, or http to a loopback address (a local
// relay or a test receiver).
func outboundURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL, got %q", key, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("%s must use https (http only to a loopback address), got %q",
		key, raw)
}
