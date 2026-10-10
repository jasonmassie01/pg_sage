package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// GCPTokenSource supplies Google access tokens (GCPToken implements it).
type GCPTokenSource interface {
	Token(ctx context.Context) (string, error)
}

// GCPOptions identify one Cloud SQL instance: by name, or by the private
// or public IP pg_sage connects to (matched through the Admin API).
type GCPOptions struct {
	Project    string
	Instance   string
	HostIP     string
	Token      GCPTokenSource
	HTTPClient *http.Client
	// Endpoint overrides (tests, Private Service Connect).
	MonitoringEndpoint, SQLAdminEndpoint string
	Now                                  func() time.Time
}

var (
	gcpProjectPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	gcpInstancePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,97}$`)
)

// GCPSource collects Cloud Monitoring telemetry for a Cloud SQL instance.
type GCPSource struct {
	opts GCPOptions
	mu   sync.Mutex
	name string // resolved instance name
}

// NewGCPSource validates the identity; it makes no network call.
func NewGCPSource(opts GCPOptions) (*GCPSource, error) {
	switch {
	case !gcpProjectPattern.MatchString(opts.Project):
		return nil, fmt.Errorf("%w: invalid Google Cloud project %q", ErrIdentity, opts.Project)
	case opts.Instance != "" && !gcpInstancePattern.MatchString(opts.Instance):
		return nil, fmt.Errorf("%w: invalid Cloud SQL instance %q", ErrIdentity, opts.Instance)
	case opts.Instance == "" && net.ParseIP(opts.HostIP) == nil:
		return nil, fmt.Errorf("%w: name the Cloud SQL instance or connect by its IP address",
			ErrIdentity)
	case opts.Token == nil:
		return nil, fmt.Errorf("%w: no Google token source", ErrNoCredentials)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MonitoringEndpoint == "" {
		opts.MonitoringEndpoint = "https://monitoring.googleapis.com"
	}
	if opts.SQLAdminEndpoint == "" {
		opts.SQLAdminEndpoint = "https://sqladmin.googleapis.com"
	}
	opts.MonitoringEndpoint = strings.TrimRight(opts.MonitoringEndpoint, "/")
	opts.SQLAdminEndpoint = strings.TrimRight(opts.SQLAdminEndpoint, "/")
	return &GCPSource{opts: opts, name: opts.Instance}, nil
}

// Provider is "cloud-sql".
func (s *GCPSource) Provider() string { return "cloud-sql" }

// get sends an authorized GET and returns the body of a 200 response.
func (s *GCPSource) get(ctx context.Context, rawURL, api string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := s.opts.Token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build %s request", ErrProvider, api)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s unreachable; check network access", ErrProvider, api)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: %s response unreadable or too large", ErrMalformed, api)
	}
	if err := checkServerDate(resp.Header.Get("Date"), s.opts.Now()); err != nil {
		return nil, fmt.Errorf("%s: %w", api, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, googleAPIError(api, resp.StatusCode, raw)
	}
	return raw, nil
}

func googleAPIError(api string, status int, raw []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	detail := fmt.Sprintf("%s HTTP %d %s: %s", api, status, sanitize(e.Error.Status),
		sanitize(e.Error.Message))
	switch {
	case status == http.StatusTooManyRequests || e.Error.Status == "RESOURCE_EXHAUSTED":
		return fmt.Errorf("%w: %s", ErrThrottled, detail)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuth, detail)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrIdentity, detail)
	}
	return fmt.Errorf("%w: %s", ErrProvider, detail)
}

// Collect gathers one sample of the instance and its replicas' lag.
func (s *GCPSource) Collect(ctx context.Context, now time.Time) (Sample, error) {
	inst, err := s.instance(ctx)
	if err != nil {
		return Sample{}, err
	}
	primary := s.opts.Project + ":" + inst.Name
	series, err := s.timeSeries(ctx, primary, inst.replicaIDs(s.opts.Project), now)
	if err != nil {
		return Sample{}, err
	}
	sample := Sample{Provider: "cloud-sql", Resource: "cloud-sql:" + primary, CollectedAt: now}
	if err := applyMonitoring(&sample, series, primary, len(inst.ReplicaNames) > 0,
		now); err != nil {
		return Sample{}, err
	}
	applyCloudSQLStorage(&sample, inst, now)
	sample.Backup = inst.Settings.BackupConfiguration.posture(inst.Settings.DeletionProtection)
	if sample.MemoryTotalBytes == nil {
		if mem := tierMemoryBytes(inst.Settings.Tier); mem > 0 {
			sample.MemoryTotalBytes = &Point{Value: mem, At: now}
			sample.MemoryTotalSource = MemorySourceMachineTier
		} else {
			sample.Missing = append(sample.Missing, "memory_total: no memory/quota metric "+
				"and unknown tier "+sanitize(inst.Settings.Tier))
		}
	}
	return sample, nil
}

// Target reads the instance's database flags (managedparam.Resolver).
func (s *GCPSource) Target(ctx context.Context) (managedparam.Target, error) {
	inst, err := s.instance(ctx)
	if err != nil {
		return managedparam.Target{}, err
	}
	flags := map[string]string{}
	for _, f := range inst.Settings.DatabaseFlags {
		flags[f.Name] = f.Value
	}
	return managedparam.Target{Provider: "cloud-sql", Project: s.opts.Project,
		InstanceID: inst.Name, Flags: flags, FlagsKnown: true, Known: true}, nil
}
