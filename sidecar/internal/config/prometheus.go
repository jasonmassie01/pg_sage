package config

import "fmt"

// DefaultPrometheusRefreshSeconds is how often each database's /metrics
// values are re-read when prometheus.refresh_interval_seconds is 0.
const DefaultPrometheusRefreshSeconds = 30

// PrometheusConfig is the /metrics listener. Scrapes render values the
// per-database refreshers read on their own cadence (observability-03).
type PrometheusConfig struct {
	ListenAddr             string `yaml:"listen_addr"`
	RefreshIntervalSeconds int    `yaml:"refresh_interval_seconds" doc:"Seconds between re-reads of each database's /metrics values; scrapes render the last read, up to one interval old. 0 means 30; never faster than collector.interval_seconds. Requires restart."`
}

func (p PrometheusConfig) validate() error {
	if p.RefreshIntervalSeconds < 0 {
		return fmt.Errorf("prometheus.refresh_interval_seconds must be 0 (default %d) "+
			"or positive, got %d", DefaultPrometheusRefreshSeconds, p.RefreshIntervalSeconds)
	}
	return nil
}
