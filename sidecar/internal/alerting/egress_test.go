package alerting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/notify"
)

func TestEgressWebhookDeliversToLoopback(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// Loopback is below the notification policy's default (alerting-11), so
	// this is the private-relay case: the egress floor itself allows it.
	relay := notify.TargetPolicy{AllowPrivate: true}.HTTPClient(HTTPTimeout)
	ch := NewWebhook("ops", srv.URL, nil, relay, func(string, string, ...any) {})
	alert := Alert{Severity: "warning", Findings: []AlertFinding{{Title: "disk"}}}
	err := ch.Send(context.Background(), alert)
	if err != nil {
		t.Fatalf("webhook to 127.0.0.1: %v", err)
	}
	if !strings.Contains(got, "disk") {
		t.Fatalf("delivered body %q lacks the alert", got)
	}
}

func TestEgressAlertChannelsRefuseMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logFn := func(string, string, ...any) {}
	webhook := NewWebhook("imds", "http://169.254.169.254/latest/meta-data/", nil, nil, logFn)
	if err := webhook.Send(ctx, Alert{Severity: "warning"}); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("webhook to metadata = %v, want egress.ErrBlocked", err)
	}
	slack := NewSlack("http://[fd00:ec2::254]/hook", nil, logFn)
	if err := slack.Send(ctx, Alert{Severity: "warning"}); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("slack to IMDS v6 = %v, want egress.ErrBlocked", err)
	}
}

// A blocked destination never becomes reachable: no retries, no backoff.
func TestEgressBlockedIsNotRetryable(t *testing.T) {
	if retryable(fmt.Errorf("slack http post: %w", egress.ErrBlocked)) {
		t.Fatal("egress.ErrBlocked was treated as retryable")
	}
	start := time.Now()
	slack := NewSlack("http://169.254.169.254/hook", nil, func(string, string, ...any) {})
	if err := slack.Send(context.Background(), Alert{Severity: "warning"}); err == nil {
		t.Fatal("slack to metadata succeeded")
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("blocked send took %v; it was retried with backoff", elapsed)
	}
}
