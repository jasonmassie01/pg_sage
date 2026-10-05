package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Cloud Monitoring timeSeries.list for the cloudsql_database resource:
// one request per metric (a filter may match only one metric type), each
// covering the instance and its replicas.

const cloudSQLMetric = "cloudsql.googleapis.com/database/"

var cloudSQLMetrics = []string{"cpu/utilization", "memory/quota", "memory/usage",
	"disk/quota", "disk/bytes_used", "disk/read_ops_count", "disk/write_ops_count",
	"postgresql/num_backends", "replication/replica_lag"}

type tsSeries struct {
	Metric struct {
		Type string `json:"type"`
	} `json:"metric"`
	Resource struct {
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	Points []struct {
		Interval struct {
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
		} `json:"interval"`
		Value struct {
			DoubleValue *float64 `json:"doubleValue"`
			Int64Value  *string  `json:"int64Value"`
		} `json:"value"`
	} `json:"points"`
}

func quoteList(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = strconv.Quote(v)
	}
	return strings.Join(q, ",")
}

func (s *GCPSource) timeSeries(ctx context.Context, primary string, replicas []string,
	now time.Time) ([]tsSeries, error) {
	ids := quoteList(append([]string{primary}, replicas...))
	var out []tsSeries
	for _, m := range cloudSQLMetrics {
		series, err := s.metricSeries(ctx, cloudSQLMetric+m, ids, now)
		if err != nil {
			return nil, err
		}
		out = append(out, series...)
	}
	return out, nil
}

func (s *GCPSource) metricSeries(ctx context.Context, metric, ids string,
	now time.Time) ([]tsSeries, error) {
	filter := fmt.Sprintf(`metric.type = %s AND resource.labels.database_id = one_of(%s)`,
		strconv.Quote(metric), ids)
	q := url.Values{"filter": {filter},
		"interval.startTime": {now.Add(-cloudWatchWindow).UTC().Format(time.RFC3339)},
		"interval.endTime":   {now.Add(time.Minute).UTC().Format(time.RFC3339)}}
	u := s.opts.MonitoringEndpoint + "/v3/projects/" + url.PathEscape(s.opts.Project) +
		"/timeSeries?" + q.Encode()
	raw, err := s.get(ctx, u, "Cloud Monitoring timeSeries.list")
	if err != nil {
		return nil, err
	}
	var resp struct {
		TimeSeries []tsSeries `json:"timeSeries"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: Cloud Monitoring JSON", ErrMalformed)
	}
	return resp.TimeSeries, nil
}

// newestSeriesPoint is a series' newest fresh point and, for DELTA
// metrics, its interval in seconds.
func newestSeriesPoint(ts tsSeries, now time.Time) (*Point, float64, error) {
	var points []Point
	intervals := map[time.Time]float64{}
	for _, p := range ts.Points {
		end, err := time.Parse(time.RFC3339Nano, p.Interval.EndTime)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: Cloud Monitoring point time", ErrMalformed)
		}
		var v float64
		switch {
		case p.Value.DoubleValue != nil:
			v = *p.Value.DoubleValue
		case p.Value.Int64Value != nil:
			n, err := strconv.ParseInt(*p.Value.Int64Value, 10, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("%w: Cloud Monitoring int64 value", ErrMalformed)
			}
			v = float64(n)
		default:
			continue
		}
		if start, err := time.Parse(time.RFC3339Nano, p.Interval.StartTime); err == nil &&
			end.After(start) {
			intervals[end] = end.Sub(start).Seconds()
		}
		points = append(points, Point{Value: v, At: end})
	}
	best, err := newestPoint(points, now)
	if err != nil || best == nil {
		return nil, 0, err
	}
	interval := intervals[best.At]
	if interval <= 0 {
		interval = 60
	}
	return best, interval, nil
}
