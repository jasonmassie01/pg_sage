package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Performance Insights GetResourceMetrics (JSON 1.1, SigV4 service "pi"):
// average active sessions (DB load), split by wait event type, and the
// Enhanced Monitoring memory total when that is enabled.

type piResult struct {
	load     *Point
	byWait   map[string]float64
	memTotal *Point // bytes
}

type piResponse struct {
	MetricList []struct {
		Key struct {
			Metric     string            `json:"Metric"`
			Dimensions map[string]string `json:"Dimensions"`
		} `json:"Key"`
		DataPoints []struct {
			Timestamp float64  `json:"Timestamp"`
			Value     *float64 `json:"Value"`
		} `json:"DataPoints"`
	} `json:"MetricList"`
}

func (c *awsClient) resourceMetrics(ctx context.Context, endpoint, resourceID string,
	now time.Time) (piResult, error) {
	req := map[string]any{"ServiceType": "RDS", "Identifier": resourceID,
		"StartTime": now.Add(-cloudWatchWindow).Unix(), "EndTime": now.Unix(),
		"PeriodInSeconds": 60, "MetricQueries": []map[string]any{
			{"Metric": "db.load.avg"},
			{"Metric": "db.load.avg", "GroupBy": map[string]any{
				"Group": "db.wait_event_type", "Limit": 10}},
			{"Metric": "os.memory.total.avg"},
		}}
	body, err := json.Marshal(req)
	if err != nil {
		return piResult{}, fmt.Errorf("%w: encode PI request", ErrProvider)
	}
	raw, err := c.do(ctx, "pi", endpoint, "application/x-amz-json-1.1",
		map[string]string{"X-Amz-Target": "PerformanceInsightsv20180227.GetResourceMetrics"},
		body)
	if err != nil {
		return piResult{}, fmt.Errorf("performance insights GetResourceMetrics: %w", err)
	}
	var resp piResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return piResult{}, fmt.Errorf("%w: Performance Insights JSON", ErrMalformed)
	}
	return parsePI(resp, now)
}

func parsePI(resp piResponse, now time.Time) (piResult, error) {
	res := piResult{byWait: map[string]float64{}}
	for _, m := range resp.MetricList {
		var points []Point
		for _, dp := range m.DataPoints {
			if dp.Value == nil || math.IsNaN(*dp.Value) || *dp.Value < 0 {
				continue
			}
			sec, frac := math.Modf(dp.Timestamp)
			points = append(points, Point{Value: *dp.Value,
				At: time.Unix(int64(sec), int64(frac*1e9)).UTC()})
		}
		p, err := newestPoint(points, now)
		if err != nil {
			return piResult{}, err
		}
		if p == nil {
			continue
		}
		switch wait := m.Key.Dimensions["db.wait_event_type.name"]; {
		case m.Key.Metric == "os.memory.total.avg":
			res.memTotal = &Point{Value: p.Value * 1024, At: p.At} // KB
		case m.Key.Metric == "db.load.avg" && wait != "":
			res.byWait[wait] = p.Value
		case m.Key.Metric == "db.load.avg":
			res.load = p
		}
	}
	return res, nil
}
