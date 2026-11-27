package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The Microsoft.Insights metric names read, checked against Microsoft's
// supported-metrics list on 2026-11-12 (azure_source_test pins them). There
// is no storage limit metric: the capacity is storage_used + storage_free.
var azurePrimaryMetrics = []string{"cpu_percent", "memory_percent", "storage_used",
	"storage_free", "active_connections", "read_iops", "write_iops"}

const azureReplicaLagMetric = "physical_replication_delay_in_seconds"

const (
	azureReaderRole     = "Reader (on the flexible server)"
	azureMonitoringRole = "Monitoring Reader (on the flexible server)"
)

type azureServerInfo struct {
	SKU struct {
		Name string `json:"name"`
	} `json:"sku"`
	Properties struct {
		Storage struct {
			AutoGrow string `json:"autoGrow"`
		} `json:"storage"`
	} `json:"properties"`
}

// Collect reads the server, its replicas, then the server's metrics and
// each replica's lag in parallel.
func (s *AzureSource) Collect(ctx context.Context, now time.Time) (Sample, error) {
	var info azureServerInfo
	if err := s.getJSON(ctx, s.serverID(), url.Values{}, azureServerAPIVersion,
		"ARM flexibleServers.get", azureReaderRole, &info); err != nil {
		return Sample{}, err
	}
	replicas, err := s.replicaIDs(ctx)
	if err != nil {
		return Sample{}, err
	}
	ids := append([]string{s.serverID()}, replicas...)
	results := make([]map[string]*Point, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names, agg := azurePrimaryMetrics, "Average"
			if i > 0 {
				names, agg = []string{azureReplicaLagMetric}, "Maximum"
			}
			results[i], errs[i] = s.metrics(ctx, id, names, agg, now)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return Sample{}, err
		}
	}
	r := azureReadings{sku: info.SKU.Name, primary: results[0], replicas: len(replicas),
		autoGrow: strings.EqualFold(info.Properties.Storage.AutoGrow, "Enabled")}
	for _, m := range results[1:] {
		if p := m[azureReplicaLagMetric]; p != nil && (r.lag == nil || p.Value > r.lag.Value) {
			r.lag = p
		}
	}
	return r.sample("azure:"+strings.ToLower(s.serverID()), now), nil
}

func (s *AzureSource) getJSON(ctx context.Context, path string, q url.Values, version,
	api, role string, out any) error {
	q.Set("api-version", version)
	raw, err := s.get(ctx, path, q, api, role)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s JSON", ErrMalformed, api)
	}
	return nil
}

// replicaIDs lists the server's read replicas, rebuilt from validated
// names (a resource ID from the response is never used as a path).
func (s *AzureSource) replicaIDs(ctx context.Context) ([]string, error) {
	var resp struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := s.getJSON(ctx, s.serverID()+"/replicas", url.Values{}, azureServerAPIVersion,
		"ARM flexibleServers.replicas", azureReaderRole, &resp); err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range resp.Value {
		m := azureServerIDPattern.FindStringSubmatch(r.ID)
		if m == nil || !azureSubscriptionPattern.MatchString(strings.ToLower(m[1])) ||
			!azureGroupPattern.MatchString(m[2]) || !azureServerPattern.MatchString(m[3]) {
			return nil, fmt.Errorf("%w: ARM replica id %q", ErrMalformed, sanitize(r.ID))
		}
		if !strings.EqualFold(m[3], s.opts.Server) {
			ids = append(ids, azureServerID(strings.ToLower(m[1]), m[2], m[3]))
		}
	}
	return ids, nil
}

type azureMetricsResponse struct {
	Value []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
		Timeseries []struct {
			Data []struct {
				TimeStamp string   `json:"timeStamp"`
				Average   *float64 `json:"average"`
				Maximum   *float64 `json:"maximum"`
			} `json:"data"`
		} `json:"timeseries"`
	} `json:"value"`
}

// metrics reads names of resource id over the freshness window at one
// minute grain and returns each metric's newest fresh point (by agg).
func (s *AzureSource) metrics(ctx context.Context, id string, names []string, agg string,
	now time.Time) (map[string]*Point, error) {
	q := url.Values{"metricnames": {strings.Join(names, ",")}, "interval": {"PT1M"},
		"aggregation": {"Average,Maximum"}, "timespan": {
			now.Add(-cloudWatchWindow).UTC().Format(time.RFC3339) + "/" +
				now.Add(time.Minute).UTC().Format(time.RFC3339)}}
	var resp azureMetricsResponse
	if err := s.getJSON(ctx, id+"/providers/Microsoft.Insights/metrics", q,
		azureMetricsAPIVersion, "Azure Monitor metrics", azureMonitoringRole,
		&resp); err != nil {
		return nil, err
	}
	out := map[string]*Point{}
	for _, m := range resp.Value {
		var points []Point
		for _, ts := range m.Timeseries {
			for _, d := range ts.Data {
				v, alt := d.Average, d.Maximum
				if agg == "Maximum" {
					v, alt = d.Maximum, d.Average
				}
				if v == nil {
					v = alt
				}
				if v == nil {
					continue
				}
				at, err := time.Parse(time.RFC3339Nano, d.TimeStamp)
				if err != nil {
					return nil, fmt.Errorf("%w: Azure Monitor timestamp", ErrMalformed)
				}
				points = append(points, Point{Value: *v, At: at})
			}
		}
		p, err := newestPoint(points, now)
		if err != nil {
			return nil, err
		}
		out[m.Name.Value] = p
	}
	return out, nil
}
