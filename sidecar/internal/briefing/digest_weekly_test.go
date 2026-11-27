package briefing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// ux-09b: the digest carries the weekly line "N messages, M led to an
// action" (the last 7 days). A failed read sends the digest without it.
// No concurrent access test: the line is read once per digest send.

func weeklyWorker(fn WeeklyLineFunc, logs *[]string) *Worker {
	cfg := &config.Config{Briefing: config.BriefingConfig{Schedule: "0 6 * * *"}}
	logFn := func(level, format string, args ...any) {
		*logs = append(*logs, level+" "+format)
	}
	return New(nil, cfg, nil, logFn).WithWeeklyLine(fn)
}

func sampleSend() digestSend {
	return digestSend{scope: "db:prod", database: "prod", hash: "h1", headline: "quiet",
		text: "Digest body"}
}

func TestDigestEvent_CarriesTheWeeklyLine(t *testing.T) {
	var logs []string
	w := weeklyWorker(func(context.Context) (string, error) {
		return "4 messages, 1 led to an action", nil
	}, &logs)
	evt := w.digestEventFor(context.Background(), sampleSend())
	if !strings.HasPrefix(evt.Body, "Digest body") ||
		!strings.Contains(evt.Body, "Last 7 days: 4 messages, 1 led to an action") {
		t.Fatalf("digest body %q lacks the weekly line", evt.Body)
	}
	if evt.Type != notify.EventDigestReady || evt.DedupKey != "digest:db:prod:h1" {
		t.Fatalf("the weekly line changed the digest's identity: %+v", evt)
	}
}

func TestDigestEvent_NoMessagesSaysSo(t *testing.T) {
	var logs []string
	w := weeklyWorker(func(context.Context) (string, error) {
		return "No messages this week", nil
	}, &logs)
	if body := w.digestEventFor(context.Background(), sampleSend()).Body; !strings.Contains(
		body, "Last 7 days: No messages this week") || strings.Contains(body, "0 of 0") {
		t.Fatalf("digest body %q", body)
	}
}

func TestDigestEvent_FailedWeeklyLineSendsTheDigestAndLogs(t *testing.T) {
	var logs []string
	w := weeklyWorker(func(context.Context) (string, error) {
		return "", errors.New("statement timeout")
	}, &logs)
	evt := w.digestEventFor(context.Background(), sampleSend())
	if evt.Body != "Digest body" {
		t.Fatalf("digest body %q, want the digest without the line", evt.Body)
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "WARN") ||
		!strings.Contains(logs[0], "weekly line") {
		t.Fatalf("logs %q, want one WARN naming the weekly line", logs)
	}
}

func TestDigestEvent_WithoutAWeeklyLineIsUnchanged(t *testing.T) {
	var logs []string
	w := weeklyWorker(nil, &logs)
	if evt := w.digestEventFor(context.Background(), sampleSend()); evt.Body != "Digest body" {
		t.Fatalf("digest body %q", evt.Body)
	}
}
