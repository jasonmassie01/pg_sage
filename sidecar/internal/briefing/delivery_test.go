package briefing

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06: Dispatch reports each channel's outcome so it can be stored in
// sage.briefings.delivery_status.

// No concurrent access tests here: Dispatch's shared state (the stdio
// notice) is covered by TestDispatchStdioNoticeOnceUnderConcurrency.

const secretHookPath = "/services/T000/B000/llm06SECRETtoken"

func slackWorker(url string, channels ...string) *Worker {
	return &Worker{cfg: &config.Config{Briefing: config.BriefingConfig{
		Channels: channels, SlackWebhookURL: url}}, logFn: noopLog,
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
}

// Error propagation: a 500 is an error with its HTTP status, never the URL.
func TestDispatchRecordsSlackRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	got := slackWorker(srv.URL+secretHookPath, "slack").Dispatch(context.Background(), "b")
	d, ok := got["slack"]
	if !ok || len(got) != 1 {
		t.Fatalf("delivery = %+v, want exactly one slack entry", got)
	}
	if d.Status != DeliveryError || d.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("slack delivery = %+v, want error with HTTP 500", d)
	}
	if !strings.Contains(d.Error, "500") || strings.Contains(d.Error, "SECRET") ||
		strings.Contains(d.Error, srv.URL) {
		t.Fatalf("error text %q must name the status and not the webhook URL", d.Error)
	}
	if d.At.IsZero() {
		t.Fatal("delivery has no time")
	}
}

// Error propagation: a refused connection is an error without the URL path.
func TestDispatchRecordsSlackTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + secretHookPath
	srv.Close()
	d := slackWorker(url, "slack").Dispatch(context.Background(), "b")["slack"]
	if d.Status != DeliveryError || d.HTTPStatus != 0 || d.Error == "" {
		t.Fatalf("slack delivery = %+v, want a transport error", d)
	}
	if strings.Contains(d.Error, "SECRET") || strings.Contains(d.Error, "/services/") {
		t.Fatalf("error text %q leaks the webhook path", d.Error)
	}
}

// Happy path: every configured channel reports; slack without a URL and an
// unknown channel are skipped with a reason.
func TestDispatchReportsEveryChannel(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	w := slackWorker(srv.URL, "stdout", "slack", "pager")
	got := w.Dispatch(context.Background(), "briefing text")
	if got["stdout"].Status != DeliverySent || got["slack"].Status != DeliverySent ||
		got["slack"].HTTPStatus != http.StatusOK {
		t.Fatalf("delivery = %+v, want stdout and slack sent", got)
	}
	if got["pager"].Status != DeliverySkipped || got["pager"].Error == "" {
		t.Fatalf("unknown channel = %+v, want skipped with a reason", got["pager"])
	}
	if calls.Load() != 1 {
		t.Fatalf("webhook calls = %d, want 1", calls.Load())
	}
	noURL := slackWorker("", "slack").Dispatch(context.Background(), "b")["slack"]
	if noURL.Status != DeliverySkipped || !strings.Contains(noURL.Error, "slack_webhook_url") {
		t.Fatalf("slack without URL = %+v, want skipped naming slack_webhook_url", noURL)
	}
}

// Nil/empty: no channels is an empty, non-nil report.
func TestDispatchWithoutChannelsReportsNothing(t *testing.T) {
	got := slackWorker("").Dispatch(context.Background(), "b")
	if got == nil || len(got) != 0 {
		t.Fatalf("delivery = %#v, want an empty map", got)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// Error propagation: a failed stdout write is an error, not "sent".
func TestDispatchRecordsStdoutWriteFailure(t *testing.T) {
	w := slackWorker("", "stdout")
	w.stdout = failingWriter{}
	d := w.Dispatch(context.Background(), "b")["stdout"]
	if d.Status != DeliveryError || !strings.Contains(d.Error, "pipe closed") {
		t.Fatalf("stdout delivery = %+v, want error naming the write failure", d)
	}
}

// Nil: recording without a pool or a stored row is a no-op error, not a panic.
func TestRecordDeliveryWithoutRow(t *testing.T) {
	w := &Worker{logFn: noopLog}
	if err := w.RecordDelivery(context.Background(), 0, nil); err == nil {
		t.Fatal("RecordDelivery with id 0 returned nil, want an error")
	}
}
