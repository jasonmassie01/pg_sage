package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// noCredsSource always fails the way the AWS chain does without
// credentials.
type noCredsSource struct{}

func (noCredsSource) Provider() string { return "rds" }

func (noCredsSource) Collect(context.Context, time.Time) (cloudtel.Sample, error) {
	return cloudtel.Sample{}, fmt.Errorf("%w: AWS default chain found nothing",
		cloudtel.ErrNoCredentials)
}

// CHECK-03: GET /api/v1/cloud-telemetry shows when the next poll is due
// and how many polls failed in a row, so a backing-off runtime is
// visible. No database: a nil manager accepts every database name.
func TestCloudTelemetryStatusShowsBackoff(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	armed := make(chan time.Duration, 1)
	rt, err := cloudtel.NewRuntime(noCredsSource{}, cloudtel.RuntimeOptions{
		Database: "backoff-db", Interval: time.Minute, Limits: cloudtel.DefaultLimits(),
		Now: func() time.Time { return now },
		After: func(d time.Duration) <-chan time.Time {
			armed <- d
			return make(chan time.Time) // never fires: Run stays in its backoff
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rt.Run(ctx, nil, nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case d := <-armed:
		if d != time.Minute {
			t.Fatalf("first backoff = %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not arm its retry")
	}
	cloudtel.Register(rt)
	t.Cleanup(func() { cloudtel.Unregister(rt) })
	code, body := serveManaged(t, cloudTelemetryHandler(nil), "GET",
		"/api/v1/cloud-telemetry?database=backoff-db", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 {
		t.Fatalf("status = %d %v", code, body)
	}
	st, _ := dbs[0].(map[string]any)
	if st["consecutive_failures"] != float64(1) || st["available"] != false {
		t.Fatalf("status = %v", st)
	}
	next, err := time.Parse(time.RFC3339, fmt.Sprint(st["next_poll_at"]))
	if err != nil || !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("next_poll_at = %v (%v), want %v", st["next_poll_at"], err,
			now.Add(time.Minute))
	}
}
