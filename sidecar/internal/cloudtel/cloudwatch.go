package cloudtel

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// CloudWatch GetMetricData over the Query protocol (form request, XML
// response), signed with SigV4 for the "monitoring" service.

const cloudWatchWindow = 15 * time.Minute

type cwQuery struct {
	id, metric, dimension, value string
}

type cwResponse struct {
	Results []struct {
		ID         string   `xml:"Id"`
		StatusCode string   `xml:"StatusCode"`
		Timestamps []string `xml:"Timestamps>member"`
		Values     []string `xml:"Values>member"`
	} `xml:"GetMetricDataResult>MetricDataResults>member"`
}

func cloudWatchForm(queries []cwQuery, now time.Time) url.Values {
	form := url.Values{"Action": {"GetMetricData"}, "Version": {"2010-08-01"},
		"StartTime": {now.Add(-cloudWatchWindow).UTC().Format(time.RFC3339)},
		"EndTime":   {now.Add(time.Minute).UTC().Format(time.RFC3339)},
		"ScanBy":    {"TimestampDescending"}}
	for i, q := range queries {
		p := fmt.Sprintf("MetricDataQueries.member.%d.", i+1)
		form.Set(p+"Id", q.id)
		form.Set(p+"ReturnData", "true")
		form.Set(p+"MetricStat.Metric.Namespace", "AWS/RDS")
		form.Set(p+"MetricStat.Metric.MetricName", q.metric)
		form.Set(p+"MetricStat.Metric.Dimensions.member.1.Name", q.dimension)
		form.Set(p+"MetricStat.Metric.Dimensions.member.1.Value", q.value)
		form.Set(p+"MetricStat.Period", "60")
		form.Set(p+"MetricStat.Stat", "Average")
	}
	return form
}

// getMetricData returns each query's datapoints, newest first.
func (c *awsClient) getMetricData(ctx context.Context, endpoint string, queries []cwQuery,
	now time.Time) (map[string][]Point, error) {
	body := []byte(cloudWatchForm(queries, now).Encode())
	raw, err := c.do(ctx, "monitoring", endpoint,
		"application/x-www-form-urlencoded; charset=utf-8", nil, body)
	if err != nil {
		return nil, fmt.Errorf("CloudWatch GetMetricData: %w", err)
	}
	var resp cwResponse
	if err := xml.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: CloudWatch GetMetricData XML", ErrMalformed)
	}
	out := make(map[string][]Point, len(resp.Results))
	for _, r := range resp.Results {
		if len(r.Timestamps) != len(r.Values) {
			return nil, fmt.Errorf("%w: CloudWatch %s has %d timestamps and %d values",
				ErrMalformed, r.ID, len(r.Timestamps), len(r.Values))
		}
		points := make([]Point, 0, len(r.Values))
		for i := range r.Values {
			at, err := time.Parse(time.RFC3339, r.Timestamps[i])
			v, verr := strconv.ParseFloat(r.Values[i], 64)
			if err != nil || verr != nil {
				return nil, fmt.Errorf("%w: CloudWatch %s datapoint", ErrMalformed, r.ID)
			}
			points = append(points, Point{Value: v, At: at})
		}
		out[r.ID] = points
	}
	return out, nil
}

// newestPoint picks the newest datapoint inside the freshness window. A
// point beyond the clock-skew tolerance in the future fails the whole
// collection: freshness can no longer be trusted.
func newestPoint(points []Point, now time.Time) (*Point, error) {
	var best *Point
	for i := range points {
		p := points[i]
		if p.At.After(now.Add(MaxClockSkew)) {
			return nil, fmt.Errorf("%w: datapoint at %s is %s in the future", ErrClockSkew,
				p.At.UTC().Format(time.RFC3339), p.At.Sub(now).Round(time.Second))
		}
		if now.Sub(p.At) > MaxPointAge {
			continue
		}
		if best == nil || p.At.After(best.At) {
			best = &p
		}
	}
	return best, nil
}
