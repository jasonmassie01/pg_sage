package config

import (
	"fmt"
	"net"
	"strconv"
)

// DefaultPrometheusRefreshSeconds is how often each database's /metrics
// values are re-read when prometheus.refresh_interval_seconds is 0.
const DefaultPrometheusRefreshSeconds = 30

// PrometheusConfig is where /metrics is served. Scrapes render values the
// per-database refreshers read on their own cadence (observability-03).
// ListenAddr is the separate, unauthenticated listener; empty opens none.
// ServeOnAPI also serves GET /metrics on the API listener, behind a
// metrics-scoped token (observability-08).
type PrometheusConfig struct {
	ListenAddr             string `yaml:"listen_addr" doc:"Address of the separate, unauthenticated /metrics listener; empty opens none. Requires restart."`
	ServeOnAPI             bool   `yaml:"serve_on_api" doc:"Also serve GET /metrics on the API listener (and its TLS), to a token with the metrics scope or, in a Databricks App, any signed-in pg_sage role. Requires restart."`
	RefreshIntervalSeconds int    `yaml:"refresh_interval_seconds" doc:"Seconds between re-reads of each database's /metrics values; scrapes render the last read, up to one interval old. 0 means 30; never faster than collector.interval_seconds. Requires restart."`
}

// SeparateListener reports whether the separate /metrics listener is
// configured.
func (p PrometheusConfig) SeparateListener() bool {
	return p.ListenAddr != ""
}

func (p PrometheusConfig) validate() error {
	if p.ListenAddr != "" {
		if err := checkListenAddr(p.ListenAddr, 0); err != nil {
			return fmt.Errorf("prometheus.listen_addr %q: %w (or \"\" for no separate "+
				"listener)", p.ListenAddr, err)
		}
	}
	if p.RefreshIntervalSeconds < 0 {
		return fmt.Errorf("prometheus.refresh_interval_seconds must be 0 (default %d) "+
			"or positive, got %d", DefaultPrometheusRefreshSeconds, p.RefreshIntervalSeconds)
	}
	return nil
}

// checkListenAddr requires host:port with a numeric port from minPort to
// 65535; the host may be empty (every interface).
func checkListenAddr(addr string, minPort int) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not a host:port address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < minPort || n > 65535 {
		return fmt.Errorf("not a host:port address with a port from %d to 65535", minPort)
	}
	return nil
}
