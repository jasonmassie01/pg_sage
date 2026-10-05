package perfgate

import (
	"fmt"
	"slices"
	"strconv"
	"time"
)

// Gate E measured one call per endpoint, so one slow round trip on a
// loaded host failed it (GET /api/v1/trust: 1.2-2.1 s once, 0.2 s the next
// run). The harness now discards a warm-up call and charges the median of
// several calls; the report keeps the worst one.

// EnvEndpointSamples is the number of measured calls per endpoint.
const EnvEndpointSamples = "PG_SAGE_PERF_ENDPOINT_SAMPLES"

// DefaultEndpointSamples is odd, so the median is a measured call.
const DefaultEndpointSamples = 5

const maxEndpointSamples = 100

// EndpointSamplesFromEnv reads the number of measured calls per endpoint.
func EndpointSamplesFromEnv(getenv func(string) string) (int, error) {
	v := getenv(EnvEndpointSamples)
	if v == "" {
		return DefaultEndpointSamples, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxEndpointSamples {
		return 0, fmt.Errorf("%s=%q: want an integer 1-%d", EnvEndpointSamples, v,
			maxEndpointSamples)
	}
	return n, nil
}

// NewEndpoint summarizes an endpoint's measured calls: Duration is their
// median, Status the first non-200 status (0 without a call).
func NewEndpoint(path string, statuses []int, durations []time.Duration) Endpoint {
	e := Endpoint{Path: path, Samples: slices.Clone(durations)}
	for _, s := range statuses {
		if e.Status == 0 || e.Status == 200 {
			e.Status = s
		}
	}
	e.Duration = median(durations)
	return e
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// Max is the slowest measured call (Duration for a single measurement).
func (e Endpoint) Max() time.Duration {
	if len(e.Samples) == 0 {
		return e.Duration
	}
	return slices.Max(e.Samples)
}

// Calls is the number of measured calls.
func (e Endpoint) Calls() int {
	if len(e.Samples) == 0 {
		return 1
	}
	return len(e.Samples)
}
