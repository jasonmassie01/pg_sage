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
	Enabled            bool                         `yaml:"enabled" doc:"Serve the Postgres-specialist contract under /api/v1/specialist (MCP tokens only). Default: true."`
	WritesPerMinute    int                          `yaml:"writes_per_minute" doc:"Opens and remediation requests one token may make per minute, 1-10000. Default: 30."`
	ReadsPerMinute     int                          `yaml:"reads_per_minute" doc:"Status, result and stream calls one token may make per minute, 1-10000. Default: 240."`
	MaxOpenPerIdentity int                          `yaml:"max_open_per_identity" doc:"Live investigations one token may have opened at once, 1-100. Attaching to an existing investigation never counts. Default: 3."`
	MaxOpenTotal       int                          `yaml:"max_open_total" doc:"Live investigations all tokens together may have opened at once, 1-100. Default: 10."`
	MaxLiveAgeHours    int                          `yaml:"max_live_age_hours" doc:"Hours after which an opened investigation pg_sage cannot read (database gone from the fleet) stops counting against the live bounds, 1-168. A readable live one always counts. Default: 24."`
	KeepIdentifiers    bool                         `yaml:"keep_identifiers" doc:"Keep schema, table and other identifiers in results. Secrets and PII are always removed; false replaces identifiers by keyed hashes like a replay export. Default: true."`
	PagerDuty          SpecialistPagerDutyConfig    `yaml:"pagerduty"`
	Webhook            SpecialistWebhookConfig      `yaml:"webhook"`
	IncidentIO         SpecialistIncidentIOConfig   `yaml:"incidentio"`
	AzureMonitor       SpecialistAzureMonitorConfig `yaml:"azure_monitor"`
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

// SpecialistIncidentIOConfig maps incident.io incident webhooks onto the
// contract (mcp-02). Off until signing_secret is set.
type SpecialistIncidentIOConfig struct {
	SigningSecret string   `yaml:"signing_secret" doc:"Signing secret (whsec_...) of the incident.io webhook endpoint; webhook-signature must match. Empty disables the adapter." secret:"true"`
	DatabaseField string   `yaml:"database_field" doc:"incident.io custom field whose value names the database through routes. Default: Database."`
	Routes        []string `yaml:"routes" doc:"Field value to database mappings, VALUE=database or VALUE=database:family, matched without case. Unmapped incidents are answered unknown_database."`
}

// SpecialistAzureMonitorConfig maps Azure Monitor alerts (common alert
// schema) onto the contract (mcp-02). Off until shared_secret is set.
type SpecialistAzureMonitorConfig struct {
	SharedSecret string   `yaml:"shared_secret" doc:"Shared key Azure alert deliveries carry in the X-Sage-Azure-Key header or the key query parameter. Empty disables the adapter." secret:"true"`
	Resources    []string `yaml:"resources" doc:"Alert target resource id to database mappings, RESOURCE_ID=database or RESOURCE_ID=database:family, matched without case. Unmapped alerts are answered unknown_database."`
}

// Specialist defaults.
const (
	DefaultSpecialistWritesPerMinute    = 30
	DefaultSpecialistReadsPerMinute     = 240
	DefaultSpecialistMaxOpenPerIdentity = 3
	DefaultSpecialistMaxOpenTotal       = 10
	DefaultSpecialistMaxLiveAgeHours    = 24
	DefaultSpecialistWebhookTolerance   = 300
	DefaultSpecialistIncidentIOField    = "Database"
)

func defaultSpecialistConfig() SpecialistConfig {
	return SpecialistConfig{Enabled: true,
		WritesPerMinute:    DefaultSpecialistWritesPerMinute,
		ReadsPerMinute:     DefaultSpecialistReadsPerMinute,
		MaxOpenPerIdentity: DefaultSpecialistMaxOpenPerIdentity,
		MaxOpenTotal:       DefaultSpecialistMaxOpenTotal,
		MaxLiveAgeHours:    DefaultSpecialistMaxLiveAgeHours, KeepIdentifiers: true,
		Webhook: SpecialistWebhookConfig{
			TimestampToleranceSeconds: DefaultSpecialistWebhookTolerance},
		IncidentIO: SpecialistIncidentIOConfig{
			DatabaseField: DefaultSpecialistIncidentIOField}}
}

// MaxLiveAge is the age after which an unreadable opened investigation
// stops counting against the live-investigation bounds.
func (s SpecialistConfig) MaxLiveAge() time.Duration {
	return time.Duration(s.MaxLiveAgeHours) * time.Hour
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
		{"max_live_age_hours", s.MaxLiveAgeHours, 1, 168},
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
	if err := s.IncidentIO.validate(); err != nil {
		return err
	}
	if err := validateAlertRoutes("specialist.azure_monitor", "resources",
		"shared_secret", s.AzureMonitor.SharedSecret, s.AzureMonitor.Resources); err != nil {
		return err
	}
	return s.Webhook.validate()
}

func (c SpecialistIncidentIOConfig) validate() error {
	if strings.TrimSpace(c.DatabaseField) == "" {
		return fmt.Errorf("specialist.incidentio.database_field must name a custom field")
	}
	return validateAlertRoutes("specialist.incidentio", "routes", "signing_secret",
		c.SigningSecret, c.Routes)
}

// validateAlertRoutes checks an alert adapter's KEY=database[:family]
// routes and that they come with the adapter's secret; the database and
// family are checked when the adapter is built.
func validateAlertRoutes(section, key, secretKey, secret string, routes []string) error {
	if len(routes) > 0 && secret == "" {
		return fmt.Errorf("%s.%s need %s.%s", section, key, section, secretKey)
	}
	for i, entry := range routes {
		k, database, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(database) == "" {
			return fmt.Errorf("%s.%s[%d] must be KEY=database[:family], got %q", section,
				key, i, entry)
		}
	}
	return nil
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
