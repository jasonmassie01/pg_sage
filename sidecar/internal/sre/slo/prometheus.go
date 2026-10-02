package slo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prometheus connector errors; callers distinguish them with errors.Is.
// No error message carries the bearer token or the URL's query.
var (
	ErrNoData       = errors.New("query returned no data")
	ErrAmbiguous    = errors.New("query returned more than one series")
	ErrInvalidValue = errors.New("query returned an invalid value")
	ErrRejected     = errors.New("query rejected")
	ErrUnavailable  = errors.New("prometheus unavailable")
)

// Connector limits.
const (
	DefaultPromTimeout = 5 * time.Second
	MaxPromTimeout     = 60 * time.Second
	maxPromBody        = 4 << 20
)

// PromConfig configures the read-only Prometheus-compatible connector.
type PromConfig struct {
	URL         string
	BearerToken string
	Timeout     time.Duration
	// HTTPClient overrides the transport (tests); nil uses a client with
	// the timeout.
	HTTPClient *http.Client
}

// PromClient queries a Prometheus-compatible HTTP API (instant query
// and query_range). It never writes.
type PromClient struct {
	base    *url.URL
	token   string
	timeout time.Duration
	http    *http.Client
}

// NewPromClient validates the configuration: an http(s) URL without
// credentials (they come from the bearer token) and a timeout up to 60s
// (zero means 5s).
func NewPromClient(c PromConfig) (*PromClient, error) {
	u, err := url.Parse(strings.TrimRight(c.URL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: prometheus url must be http(s)://host[:port][/path]",
			ErrInvalidObjective)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: prometheus url must not carry credentials; "+
			"use the bearer token", ErrInvalidObjective)
	}
	timeout := c.Timeout
	switch {
	case timeout < 0 || timeout > MaxPromTimeout:
		return nil, fmt.Errorf("%w: prometheus timeout must be 0-60s", ErrInvalidObjective)
	case timeout == 0:
		timeout = DefaultPromTimeout
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	u.RawQuery, u.Fragment = "", ""
	return &PromClient{base: u, token: c.BearerToken, timeout: timeout, http: hc}, nil
}

// Timeout is the per-request timeout.
func (c *PromClient) Timeout() time.Duration { return c.timeout }

// Point is one sample of a range query; NaN marks an invalid value.
type Point struct {
	At    time.Time
	Value float64
}

type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Query runs an instant query at a time and returns its single value: a
// scalar or a one-series vector. Empty is ErrNoData, several series
// ErrAmbiguous, NaN, Inf or a negative count ErrInvalidValue.
func (c *PromClient) Query(ctx context.Context, q string, at time.Time) (float64, error) {
	form := url.Values{"query": {q}, "time": {formatTime(at)}}
	resp, err := c.do(ctx, "/api/v1/query", form)
	if err != nil {
		return 0, err
	}
	switch resp.Data.ResultType {
	case "scalar":
		var pair []json.RawMessage
		if json.Unmarshal(resp.Data.Result, &pair) != nil || len(pair) != 2 {
			return 0, fmt.Errorf("%w: malformed scalar", ErrUnavailable)
		}
		return sampleValue(pair[1])
	case "vector":
		var vec []struct {
			Value []json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(resp.Data.Result, &vec); err != nil {
			return 0, fmt.Errorf("%w: malformed vector", ErrUnavailable)
		}
		switch {
		case len(vec) == 0:
			return 0, ErrNoData
		case len(vec) > 1:
			return 0, fmt.Errorf("%w: %d series", ErrAmbiguous, len(vec))
		case len(vec[0].Value) != 2:
			return 0, fmt.Errorf("%w: malformed sample", ErrUnavailable)
		}
		return sampleValue(vec[0].Value[1])
	}
	return 0, fmt.Errorf("%w: unexpected result type %q", ErrUnavailable, resp.Data.ResultType)
}

// QueryRange runs a range query and returns the points of its single
// series in time order; NaN or Inf points keep a NaN value.
func (c *PromClient) QueryRange(ctx context.Context, q string, start, end time.Time,
	step time.Duration) ([]Point, error) {
	if !end.After(start) || step <= 0 {
		return nil, fmt.Errorf("%w: range end must follow its start", ErrRejected)
	}
	form := url.Values{"query": {q}, "start": {formatTime(start)}, "end": {formatTime(end)},
		"step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
	resp, err := c.do(ctx, "/api/v1/query_range", form)
	if err != nil {
		return nil, err
	}
	if resp.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("%w: unexpected result type %q", ErrUnavailable,
			resp.Data.ResultType)
	}
	var mat []struct {
		Values [][]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(resp.Data.Result, &mat); err != nil {
		return nil, fmt.Errorf("%w: malformed matrix", ErrUnavailable)
	}
	switch {
	case len(mat) == 0:
		return nil, ErrNoData
	case len(mat) > 1:
		return nil, fmt.Errorf("%w: %d series", ErrAmbiguous, len(mat))
	}
	return matrixPoints(mat[0].Values)
}

func matrixPoints(values [][]json.RawMessage) ([]Point, error) {
	out := make([]Point, 0, len(values))
	for _, pair := range values {
		if len(pair) != 2 {
			return nil, fmt.Errorf("%w: malformed sample", ErrUnavailable)
		}
		var ts float64
		if err := json.Unmarshal(pair[0], &ts); err != nil {
			return nil, fmt.Errorf("%w: malformed timestamp", ErrUnavailable)
		}
		v, err := sampleValue(pair[1])
		if err != nil {
			v = math.NaN()
		}
		out = append(out, Point{At: unixTime(ts), Value: v})
	}
	return out, nil
}

// sampleValue parses a Prometheus sample value (a JSON string).
func sampleValue(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("%w: value is not a string", ErrInvalidValue)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("%w: %.40q", ErrInvalidValue, s)
	}
	return v, nil
}

func (c *PromClient) do(ctx context.Context, path string, form url.Values) (promResponse,
	error) {
	var out promResponse
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	endpoint := c.base.JoinPath(path).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return out, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %s: %s", ErrUnavailable, c.base.Redacted(),
			transportReason(ctx, err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPromBody))
	if err != nil {
		return out, fmt.Errorf("%w: reading the response", ErrUnavailable)
	}
	return out, decodeResponse(resp.StatusCode, body, &out)
}

func decodeResponse(code int, body []byte, out *promResponse) error {
	jsonErr := json.Unmarshal(body, out)
	switch {
	case code >= 500:
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, code)
	case code >= 400:
		return fmt.Errorf("%w: HTTP %d %s", ErrRejected, code, clip(out.Error))
	case jsonErr != nil:
		return fmt.Errorf("%w: response is not JSON", ErrUnavailable)
	case out.Status != "success":
		return fmt.Errorf("%w: %s %s", ErrRejected, clip(out.ErrorType), clip(out.Error))
	}
	return nil
}

// transportReason names a transport failure without echoing the request.
func transportReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(ctx.Err(), context.Canceled):
		return "cancelled"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return clip(ue.Err.Error())
	}
	return "request failed"
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

func unixTime(ts float64) time.Time {
	sec := math.Floor(ts)
	return time.Unix(int64(sec), int64((ts-sec)*1e9)).UTC()
}
