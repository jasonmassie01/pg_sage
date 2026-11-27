package cloudtel

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// AlloyDB host telemetry from Cloud Monitoring (alloydb.googleapis.com/
// Instance, keyed on cluster_id and instance_id). Storage is the cluster's
// auto-scaling volume, so no bounded capacity exists. The metric types were
// checked against Google's metric list on 2026-11-12 (alloydb_source_test
// pins them).

const alloyDBMetric = "alloydb.googleapis.com/instance/"

var alloyDBMetrics = []string{"cpu/average_utilization", "cpu/vcpus",
	"memory/min_available_memory", "postgres/total_connections",
	"postgres/replication/maximum_lag"}

var alloyDBIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// alloyDBMemoryGiB is the memory of an AlloyDB machine by its vCPU count
// (8 GiB per vCPU for the 2-96 vCPU shapes, as of 2026-11). 1 and 128 vCPU
// shapes do not follow the ratio and are unknown here, never a guess.
var alloyDBMemoryGiB = map[float64]float64{2: 16, 4: 32, 8: 64, 16: 128, 32: 256,
	48: 384, 64: 512, 72: 576, 96: 768}

// AlloyDBMemoryBytes is the RAM of an AlloyDB node with vcpus (0: unknown).
func AlloyDBMemoryBytes(vcpus float64) float64 { return alloyDBMemoryGiB[vcpus] * gibF }

// AlloyDBOptions identify one AlloyDB instance.
type AlloyDBOptions struct {
	Project, Cluster, Instance string
	Token                      GCPTokenSource
	HTTPClient                 *http.Client
	MonitoringEndpoint         string // tests, Private Service Connect
	Now                        func() time.Time
	Sleep                      func(ctx context.Context, d time.Duration) error
	// Parallel bounds concurrent metric reads (0: one per metric).
	Parallel int
}

// AlloyDBSource collects Cloud Monitoring telemetry for an AlloyDB
// instance. It reads no flags: managed changes resolve elsewhere.
type AlloyDBSource struct{ opts AlloyDBOptions }

// NewAlloyDBSource validates the identity; it makes no network call.
func NewAlloyDBSource(opts AlloyDBOptions) (*AlloyDBSource, error) {
	switch {
	case !gcpProjectPattern.MatchString(opts.Project):
		return nil, fmt.Errorf("%w: invalid Google Cloud project %q", ErrIdentity, opts.Project)
	case !alloyDBIDPattern.MatchString(opts.Cluster):
		return nil, fmt.Errorf("%w: invalid AlloyDB cluster %q", ErrIdentity, opts.Cluster)
	case !alloyDBIDPattern.MatchString(opts.Instance):
		return nil, fmt.Errorf("%w: invalid AlloyDB instance %q", ErrIdentity, opts.Instance)
	case opts.Token == nil:
		return nil, fmt.Errorf("%w: no Google token source", ErrNoCredentials)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = egress.HTTPClient(20 * time.Second)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MonitoringEndpoint == "" {
		opts.MonitoringEndpoint = "https://monitoring.googleapis.com"
	}
	opts.MonitoringEndpoint = strings.TrimRight(opts.MonitoringEndpoint, "/")
	return &AlloyDBSource{opts: opts}, nil
}

// Provider is "alloydb".
func (s *AlloyDBSource) Provider() string { return "alloydb" }

func (s *AlloyDBSource) resource() string {
	return "alloydb:projects/" + s.opts.Project + "/clusters/" + s.opts.Cluster +
		"/instances/" + s.opts.Instance
}

// Collect reads the instance's metrics in parallel and maps them.
func (s *AlloyDBSource) Collect(ctx context.Context, now time.Time) (Sample, error) {
	mon := monitoringClient{api: gcpAPI{token: s.opts.Token, client: s.opts.HTTPClient,
		now: s.opts.Now, sleep: s.opts.Sleep}, endpoint: s.opts.MonitoringEndpoint,
		project: s.opts.Project, prefix: alloyDBMetric}
	filter := fmt.Sprintf(`resource.type = "alloydb.googleapis.com/Instance" AND `+
		`resource.labels.cluster_id = %s AND resource.labels.instance_id = %s`,
		strconv.Quote(s.opts.Cluster), strconv.Quote(s.opts.Instance))
	series := make([][]tsSeries, len(alloyDBMetrics))
	errs := make([]error, len(alloyDBMetrics))
	parallel := s.opts.Parallel
	if parallel <= 0 {
		parallel = len(alloyDBMetrics)
	}
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, m := range alloyDBMetrics {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			series[i], errs[i] = mon.series(ctx, m, filter, now)
		}()
	}
	wg.Wait()
	values := map[string]*Point{}
	for i, m := range alloyDBMetrics {
		if errs[i] != nil {
			return Sample{}, errs[i]
		}
		p, err := maxNewestPoint(series[i], now)
		if err != nil {
			return Sample{}, err
		}
		values[m] = p
	}
	return alloyDBSample(s.resource(), values, now), nil
}

// maxNewestPoint is the largest of the series' newest fresh points (the
// worst read pool's lag; other metrics have one series).
func maxNewestPoint(series []tsSeries, now time.Time) (*Point, error) {
	var best *Point
	for _, ts := range series {
		p, _, err := newestSeriesPoint(ts, now)
		if err != nil {
			return nil, err
		}
		if p != nil && (best == nil || p.Value > best.Value) {
			best = p
		}
	}
	return best, nil
}
