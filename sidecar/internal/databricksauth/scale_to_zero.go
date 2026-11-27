package databricksauth

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// EndpointInfo is what pg_sage last read about a Lakebase endpoint's compute.
// It comes from the GetEndpoint call that configures the endpoint's pools, so
// it costs no extra API call, and it is never refreshed on its own.
type EndpointInfo struct {
	// SuspendTimeout is the inactivity after which Lakebase suspends the
	// compute (scale-to-zero). 0 means off, not reported, or reported
	// invalid (negative).
	SuspendTimeout time.Duration
	// State is the compute's state at ObservedAt: ACTIVE, IDLE, INIT,
	// DEGRADED, or "" when not reported.
	State      string
	ObservedAt time.Time
	// Blockers are the pg_sage cadences, noted by its pools on this
	// endpoint, that are shorter than SuspendTimeout.
	Blockers []string
}

var endpointInfos = struct {
	mu sync.Mutex
	m  map[string]EndpointInfo // endpoint resource path -> info
}{m: map[string]EndpointInfo{}}

func endpointInfoFrom(status *postgres.EndpointStatus, now time.Time) EndpointInfo {
	suspend := status.SuspendTimeoutDuration.AsDuration() // nil-safe: 0
	if suspend < 0 {
		suspend = 0
	}
	return EndpointInfo{SuspendTimeout: suspend, State: string(status.CurrentState),
		ObservedAt: now}
}

// recordEndpointInfo stores a fresh observation and keeps the blockers that
// pools already noted: they come from pool settings, not from the endpoint.
func recordEndpointInfo(endpoint string, info EndpointInfo) {
	endpointInfos.mu.Lock()
	defer endpointInfos.mu.Unlock()
	info.Blockers = endpointInfos.m[endpoint].Blockers
	endpointInfos.m[endpoint] = info
}

// EndpointInfoFor reports the latest observation of endpoint. ok is false
// until a pool for it has been configured successfully.
func EndpointInfoFor(endpoint string) (info EndpointInfo, ok bool) {
	endpointInfos.mu.Lock()
	defer endpointInfos.mu.Unlock()
	info, ok = endpointInfos.m[endpoint]
	info.Blockers = slices.Clone(info.Blockers)
	return info, ok
}

// EndpointInfoForHost resolves host through SAGE_LAKEBASE_ENDPOINT(S) and
// reports that endpoint's info. ok is false for ordinary PostgreSQL hosts,
// a malformed mapping, and endpoints not yet observed.
func EndpointInfoForHost(host string) (endpoint string, info EndpointInfo, ok bool) {
	endpoint, err := endpointForHost(host)
	if err != nil || endpoint == "" {
		return "", EndpointInfo{}, false
	}
	info, ok = EndpointInfoFor(endpoint)
	if !ok {
		return "", EndpointInfo{}, false
	}
	return endpoint, info, true
}

// NoteScaleToZeroBlockers adds blockers to an observed endpoint and returns
// only the ones it had not noted before, so a caller warns once per blocker
// however many pools or reconnects report it. An endpoint not yet observed
// notes nothing.
func NoteScaleToZeroBlockers(endpoint string, blockers []string) []string {
	endpointInfos.mu.Lock()
	defer endpointInfos.mu.Unlock()
	info, ok := endpointInfos.m[endpoint]
	if !ok {
		return nil
	}
	var added []string
	for _, b := range blockers {
		if !slices.Contains(info.Blockers, b) {
			info.Blockers = append(info.Blockers, b)
			added = append(added, b)
		}
	}
	endpointInfos.m[endpoint] = info
	return added
}

// ScaleToZeroBlockers names each pg_sage cadence strictly shorter than the
// endpoint's suspend timeout: the collector, the pool health check (only
// while MinConns keeps connections) and MinConns itself, an always-open
// connection. It does not claim which of them Lakebase counts as activity.
// No suspend timeout, or a zero or negative cadence, names nothing.
func ScaleToZeroBlockers(
	info EndpointInfo, collector time.Duration, minConns int32, healthCheck time.Duration,
) []string {
	suspend := info.SuspendTimeout
	if suspend <= 0 {
		return nil
	}
	var out []string
	if collector > 0 && collector < suspend {
		out = append(out, fmt.Sprintf("collector every %s", collector))
	}
	if minConns > 0 {
		if healthCheck > 0 && healthCheck < suspend {
			out = append(out, fmt.Sprintf("health check every %s", healthCheck))
		}
		out = append(out, fmt.Sprintf("min_conns=%d keeps a connection open", minConns))
	}
	return out
}
