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
// ux-02b: "slack" is no longer posted by Dispatch: briefing.slack_webhook_url
// is reconciled into the Notifications channel config:slack:briefing, which
// reports under its own name. (Before, this test counted a webhook call;
// that post path is deleted.) stdout is still written; an unknown channel
// is skipped with a reason.
func TestDispatchReportsEveryChannel(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	w := slackWorker(srv.URL, "stdout", "slack", "pager")
	got := w.Dispatch(context.Background(), "briefing text")
	if got["stdout"].Status != DeliverySent {
		t.Fatalf("delivery = %+v, want stdout sent", got)
	}
	if _, posted := got["slack"]; posted || calls.Load() != 0 {
		t.Fatalf("slack = %+v with %d webhook calls, want no post from the briefing",
			got["slack"], calls.Load())
	}
	if got["pager"].Status != DeliverySkipped || got["pager"].Error == "" {
		t.Fatalf("unknown channel = %+v, want skipped with a reason", got["pager"])
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
