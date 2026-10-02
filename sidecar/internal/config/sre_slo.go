package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SRESLOConfig configures Sage SRE SLOs and burn-rate alerting (M5). SLO
// evaluation and the database proxy SLIs are on by default: read-only,
// labeled proxies, no external setup. Registered app SLIs need a
// Prometheus connector or a push secret.
type SRESLOConfig struct {
	Enabled                   bool                 `yaml:"enabled" doc:"Evaluate SLO burn rates for each database: the database proxy SLIs and any registered app SLIs. Read-only. Default: true."`
	EvaluationIntervalSeconds int                  `yaml:"evaluation_interval_seconds" doc:"Seconds between SLO evaluations, 15-600. Default: 60."`
	OpenInvestigations        bool                 `yaml:"open_investigations" doc:"A page-level burn of a registered app SLI opens a read-only slo_burn investigation (a proxy burn only with sre.automatic_start). Default: true."`
	BurnRules                 []SREBurnRuleConfig  `yaml:"burn_rules" doc:"Multi-window burn-rate rules; empty uses the Google SRE workbook defaults (page 14.4x over 1h and 5m, page 6x over 6h and 30m, ticket 1x over 3d and 6h)."`
	Prometheus                SREPrometheusConfig  `yaml:"prometheus"`
	Push                      SRESLIPushConfig     `yaml:"push"`
	Proxies                   SREProxyConfig       `yaml:"proxies"`
	Objectives                []SREObjectiveConfig `yaml:"objectives" doc:"Registered app SLIs (SLOs): only these claim customer impact."`
}

// SREBurnRuleConfig is one burn-rate rule; windows are Go durations or
// whole days ("3d").
type SREBurnRuleConfig struct {
	Severity    string  `yaml:"severity" doc:"Rule severity: page or ticket."`
	LongWindow  string  `yaml:"long_window" doc:"Long window, e.g. 1h or 3d."`
	ShortWindow string  `yaml:"short_window" doc:"Short window, shorter than the long one."`
	Factor      float64 `yaml:"factor" doc:"Burn rate both windows must reach."`
}

// SREPrometheusConfig is the read-only Prometheus-compatible SLI connector.
type SREPrometheusConfig struct {
	URL             string `yaml:"url" doc:"Prometheus-compatible HTTP API base URL (query and query_range). Empty disables the connector."`
	BearerToken     string `yaml:"bearer_token" secret:"true" doc:"Bearer token; prefer ${ENV} substitution or bearer_token_file."`
	BearerTokenFile string `yaml:"bearer_token_file" doc:"File holding the bearer token (e.g. a mounted secret), read at startup."`
	TimeoutSeconds  int    `yaml:"timeout_seconds" doc:"Per-query timeout, 1-60. Default: 5."`
}

// SRESLIPushConfig authenticates pushed SLI counters.
type SRESLIPushConfig struct {
	HMACSecret                string `yaml:"hmac_secret" secret:"true" doc:"Shared secret (at least 32 bytes) signing POST /api/v1/sre/sli/{name}. Empty disables pushes."`
	TimestampToleranceSeconds int    `yaml:"timestamp_tolerance_seconds" doc:"Accepted signature clock skew, 30-3600. Default: 300."`
}

// SREProxyConfig tunes the database proxy SLIs.
type SREProxyConfig struct {
	Enabled                     bool    `yaml:"enabled" doc:"Database proxy SLIs (latency, server-class errors, connection refusal, replication lag), always labeled proxies. Default: true."`
	Target                      float64 `yaml:"target" doc:"Proxy SLO target, in (0, 0.9999]. Default: 0.99."`
	WindowDays                  int     `yaml:"window_days" doc:"Proxy SLO window in days, 1-90. Default: 30."`
	LatencyThresholdMs          float64 `yaml:"latency_threshold_ms" doc:"Absolute p95 latency threshold; 0 = relative to the database's own last-week median. Default: 0."`
	LatencyFactor               float64 `yaml:"latency_factor" doc:"Relative threshold: this times the median interval p95, at least 1. Default: 3."`
	LatencyFloorMs              float64 `yaml:"latency_floor_ms" doc:"The relative threshold is never under this. Default: 50."`
	TopQueries                  int     `yaml:"top_queries" doc:"Top queries by interval total time in the latency proxy, 1-100. Default: 20."`
	ReplicationLagBudgetSeconds int     `yaml:"replication_lag_budget_seconds" doc:"Replay lag over this is a bad minute, 1-86400. Default: 60."`
}

// SREObjectiveConfig is one registered app SLI. Zero numbers mean the
// defaults (30-day window, 50 events, 5-minute staleness).
type SREObjectiveConfig struct {
	Name              string  `yaml:"name" doc:"Lower-case name; db_ is reserved for proxies."`
	Database          string  `yaml:"database" doc:"Monitored database the service uses; empty = every database."`
	Description       string  `yaml:"description" doc:"What the SLI measures, shown with its state."`
	Owner             string  `yaml:"owner" doc:"Team or person who owns the service SLO."`
	Source            string  `yaml:"source" doc:"Where the counters come from: prometheus or push."`
	Target            float64 `yaml:"target" doc:"SLO target in (0, 1), e.g. 0.999."`
	WindowDays        int     `yaml:"window_days" doc:"SLO window in days, 1-90. Default: 30."`
	BadQuery          string  `yaml:"bad_query" doc:"PromQL for bad events over $window (use 'or vector(0)' so zero errors is not absent data)."`
	EligibleQuery     string  `yaml:"eligible_query" doc:"PromQL for eligible events over $window."`
	ResetsQuery       string  `yaml:"resets_query" doc:"Optional PromQL counting counter resets over $window."`
	MinEligibleEvents float64 `yaml:"min_eligible_events" doc:"Fewer eligible events in a window is low traffic (unknown). Default: 50."`
	StaleAfterSeconds int     `yaml:"stale_after_seconds" doc:"Pushed samples older than this are stale. Default: 300."`
}

// SREChangeEventsConfig configures the change feed.
type SREChangeEventsConfig struct {
	HMACSecret                string   `yaml:"hmac_secret" secret:"true" doc:"Shared secret (at least 32 bytes) signing POST /api/v1/sre/change-events. Empty disables ingestion."`
	TimestampToleranceSeconds int      `yaml:"timestamp_tolerance_seconds" doc:"Accepted signature clock skew, 30-3600. Default: 300."`
	AllowedSources            []string `yaml:"allowed_sources" doc:"Sources allowed to submit events; empty allows any signed source."`
	FeedEnabled               bool     `yaml:"feed_enabled" doc:"Poll pg_sage's own change sources (actions, config audit, DDL, statistics resets, restarts, failovers, extensions). Default: true."`
	FeedIntervalSeconds       int      `yaml:"feed_interval_seconds" doc:"Seconds between change feed polls, 15-3600. Default: 60."`
	RetentionDays             int      `yaml:"retention_days" doc:"Days change events are kept, 1-3650. Default: 90."`
}

// SLO defaults.
const (
	DefaultSLOEvaluationSeconds = 60
	DefaultSLOWindowDays        = 30
	DefaultSLOMinEligible       = 50
	DefaultSLOStaleSeconds      = 300
	defaultToleranceSeconds     = 300
	minSecretBytes              = 32
)

func defaultSRESLOConfig() SRESLOConfig {
	return SRESLOConfig{Enabled: true, EvaluationIntervalSeconds: DefaultSLOEvaluationSeconds,
		OpenInvestigations: true, Prometheus: SREPrometheusConfig{TimeoutSeconds: 5},
		Push: SRESLIPushConfig{TimestampToleranceSeconds: defaultToleranceSeconds},
		Proxies: SREProxyConfig{Enabled: true, Target: 0.99, WindowDays: DefaultSLOWindowDays,
			LatencyFactor: 3, LatencyFloorMs: 50, TopQueries: 20,
			ReplicationLagBudgetSeconds: 60}}
}

func defaultSREChangeEventsConfig() SREChangeEventsConfig {
	return SREChangeEventsConfig{TimestampToleranceSeconds: defaultToleranceSeconds,
		FeedEnabled: true, FeedIntervalSeconds: 60, RetentionDays: 90}
}

// EvaluationInterval is the SLO evaluation period.
func (s SRESLOConfig) EvaluationInterval() time.Duration {
	return time.Duration(s.EvaluationIntervalSeconds) * time.Second
}

// SRERule is a parsed burn-rate rule.
type SRERule struct {
	Severity    string
	Long, Short time.Duration
	Factor      float64
}

// Rules parses the configured burn-rate rules (nil when none are set).
func (s SRESLOConfig) Rules() ([]SRERule, error) {
	var out []SRERule
	for i, r := range s.BurnRules {
		long, err := ParseWindow(r.LongWindow)
		if err != nil {
			return nil, fmt.Errorf("sre.slo.burn_rules[%d].long_window: %w", i, err)
		}
		short, err := ParseWindow(r.ShortWindow)
		if err != nil {
			return nil, fmt.Errorf("sre.slo.burn_rules[%d].short_window: %w", i, err)
		}
		out = append(out, SRERule{Severity: r.Severity, Long: long, Short: short,
			Factor: r.Factor})
	}
	return out, nil
}

var windowPattern = regexp.MustCompile(`^([1-9][0-9]*)d$`)

// ParseWindow parses a Go duration or whole days ("3d"); it must be
// positive.
func ParseWindow(s string) (time.Duration, error) {
	if m := windowPattern.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("invalid window %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid window %q (use e.g. 5m, 1h or 3d)", s)
	}
	return d, nil
}

// Token returns the bearer token, read from the file when one is set.
func (p SREPrometheusConfig) Token() (string, error) {
	if p.BearerTokenFile == "" {
		return p.BearerToken, nil
	}
	raw, err := os.ReadFile(p.BearerTokenFile)
	if err != nil {
		return "", fmt.Errorf("sre.slo.prometheus.bearer_token_file %s: %w",
			p.BearerTokenFile, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// Timeout is the per-query timeout.
func (p SREPrometheusConfig) Timeout() time.Duration {
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// Tolerance is the push signature tolerance.
func (p SRESLIPushConfig) Tolerance() time.Duration {
	return time.Duration(p.TimestampToleranceSeconds) * time.Second
}

// Window is the objective's SLO window.
func (o SREObjectiveConfig) Window() time.Duration {
	days := o.WindowDays
	if days == 0 {
		days = DefaultSLOWindowDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// MinEligible is the objective's low-traffic floor.
func (o SREObjectiveConfig) MinEligible() float64 {
	if o.MinEligibleEvents == 0 {
		return DefaultSLOMinEligible
	}
	return o.MinEligibleEvents
}

// StaleAfter is how old the newest pushed sample may be.
func (o SREObjectiveConfig) StaleAfter() time.Duration {
	secs := o.StaleAfterSeconds
	if secs == 0 {
		secs = DefaultSLOStaleSeconds
	}
	return time.Duration(secs) * time.Second
}

// Tolerance is the change-event signature tolerance.
func (c SREChangeEventsConfig) Tolerance() time.Duration {
	return time.Duration(c.TimestampToleranceSeconds) * time.Second
}

// FeedInterval is the change feed poll period.
func (c SREChangeEventsConfig) FeedInterval() time.Duration {
	return time.Duration(c.FeedIntervalSeconds) * time.Second
}

// Retention is how long change events are kept.
func (c SREChangeEventsConfig) Retention() time.Duration {
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}

func secretOK(s string) bool { return s == "" || len(s) >= minSecretBytes }

func finitePositive(f float64) bool { return f > 0 && !math.IsInf(f, 0) }

func validPromURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.User == nil
}

// cloneSRESignals deep-copies the SLO and change-event lists.
func cloneSRESignals(dst *SREConfig, src SREConfig) {
	dst.SLO.BurnRules = append([]SREBurnRuleConfig(nil), src.SLO.BurnRules...)
	dst.SLO.Objectives = append([]SREObjectiveConfig(nil), src.SLO.Objectives...)
	dst.ChangeEvents.AllowedSources = append([]string(nil),
		src.ChangeEvents.AllowedSources...)
}
