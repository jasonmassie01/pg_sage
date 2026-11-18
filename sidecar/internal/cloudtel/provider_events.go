package cloudtel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

// Provider events (guard-02b): the maintenance and failover events a
// cloud provider reports for the database's resource, for the change
// feed. Reads only; nothing is ever discovered account-wide.

// Provider event categories.
const (
	EventRestart       = "restart"
	EventFailover      = "failover"
	EventVersionChange = "version_change"
)

// ProviderEvent is one provider-reported event.
type ProviderEvent struct {
	// ID is stable across reads: provider, resource and the event itself.
	ID       string
	Category string
	Message  string
	At       time.Time
}

// EventSource is a telemetry source that also reads its provider's
// events at or after since.
type EventSource interface {
	ProviderEvents(ctx context.Context, since time.Time) ([]ProviderEvent, error)
}

// HasProviderEvents reports whether the runtime's source reads provider
// events.
func (r *Runtime) HasProviderEvents() bool {
	if !r.HasSource() {
		return false
	}
	_, ok := r.src.(EventSource)
	return ok
}

// ProviderEvents reads the source's provider events; a runtime without
// that capability is ErrUnavailable.
func (r *Runtime) ProviderEvents(ctx context.Context, since time.Time) ([]ProviderEvent,
	error) {
	if !r.HasProviderEvents() {
		return nil, fmt.Errorf("%w: no provider event source", ErrUnavailable)
	}
	return r.src.(EventSource).ProviderEvents(ctx, since)
}

// maxEventPages bounds one DescribeEvents read.
const maxEventPages = 5

// ProviderEvents reads RDS DescribeEvents (maintenance and failover) of
// the instance, or of the Aurora cluster when the source names one.
func (s *AWSSource) ProviderEvents(ctx context.Context, since time.Time) ([]ProviderEvent,
	error) {
	now := s.opts.Now()
	if !since.Before(now) {
		return nil, nil
	}
	id, kind := s.opts.InstanceID, types.SourceTypeDbInstance
	if s.opts.ClusterID != "" {
		id, kind = s.opts.ClusterID, types.SourceTypeDbCluster
	}
	in := &rds.DescribeEventsInput{SourceIdentifier: aws.String(id), SourceType: kind,
		StartTime: aws.Time(since), EndTime: aws.Time(now),
		EventCategories: []string{"maintenance", "failover"}, MaxRecords: aws.Int32(100)}
	var out []ProviderEvent
	for page := 0; page < maxEventPages; page++ {
		resp, err := s.rds.client.DescribeEvents(ctx, in)
		if err != nil {
			return nil, sdkError(ctx, "RDS DescribeEvents", err)
		}
		for _, e := range resp.Events {
			if pe, ok := rdsEvent(id, e); ok {
				out = append(out, pe)
			}
		}
		if aws.ToString(resp.Marker) == "" {
			break
		}
		in.Marker = resp.Marker
	}
	return out, nil
}

// rdsEvent maps an RDS event: failover; a maintenance patch or upgrade is
// a version change; other maintenance takes the instance offline.
func rdsEvent(id string, e types.Event) (ProviderEvent, bool) {
	msg := sanitize(aws.ToString(e.Message))
	if msg == "" || e.Date == nil {
		return ProviderEvent{}, false
	}
	var category string
	for _, c := range e.EventCategories {
		switch {
		case c == "failover":
			category = EventFailover
		case c == "maintenance" && category == "":
			category = EventRestart
			lower := strings.ToLower(msg)
			if strings.Contains(lower, "patch") || strings.Contains(lower, "upgrad") {
				category = EventVersionChange
			}
		}
	}
	if category == "" {
		return ProviderEvent{}, false
	}
	sum := sha256.Sum256([]byte(msg))
	return ProviderEvent{ID: fmt.Sprintf("rds:%s:%s:%s", id,
		e.Date.UTC().Format(time.RFC3339), hex.EncodeToString(sum[:6])),
		Category: category, Message: "RDS: " + msg, At: e.Date.UTC()}, true
}

// cloudSQLOperationKinds maps the Cloud SQL operation types that are the
// provider's, or that restart or move the server.
var cloudSQLOperationKinds = map[string]string{
	"MAINTENANCE":           EventRestart,
	"FAILOVER":              EventFailover,
	"MAJOR_VERSION_UPGRADE": EventVersionChange,
}

type sqlOperations struct {
	Items []struct {
		Name          string `json:"name"`
		OperationType string `json:"operationType"`
		Status        string `json:"status"`
		InsertTime    string `json:"insertTime"`
		StartTime     string `json:"startTime"`
	} `json:"items"`
}

// ProviderEvents reads the instance's recent Cloud SQL Admin operations
// (the newest 50) and keeps maintenance, failover and major upgrades.
func (s *GCPSource) ProviderEvents(ctx context.Context, since time.Time) ([]ProviderEvent,
	error) {
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	if name == "" {
		inst, err := s.instance(ctx)
		if err != nil {
			return nil, err
		}
		name = inst.Name
	}
	u := s.opts.SQLAdminEndpoint + "/v1/projects/" + url.PathEscape(s.opts.Project) +
		"/operations?" + url.Values{"instance": {name}, "maxResults": {"50"}}.Encode()
	raw, err := s.get(ctx, u, "Cloud SQL Admin operations.list")
	if err != nil {
		return nil, err
	}
	var ops sqlOperations
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil, fmt.Errorf("%w: Cloud SQL operations JSON", ErrMalformed)
	}
	var out []ProviderEvent
	for _, op := range ops.Items {
		category, ok := cloudSQLOperationKinds[op.OperationType]
		at := firstTime(op.StartTime, op.InsertTime)
		if !ok || op.Name == "" || at.IsZero() || at.Before(since) {
			continue
		}
		out = append(out, ProviderEvent{ID: "cloudsql:" + name + ":" + sanitize(op.Name),
			Category: category, At: at, Message: fmt.Sprintf("Cloud SQL %s operation (%s)",
				strings.ToLower(op.OperationType), sanitize(op.Status))})
	}
	return out, nil
}

// firstTime parses the first RFC 3339 time that is set (zero: none).
func firstTime(values ...string) time.Time {
	for _, v := range values {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
