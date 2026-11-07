package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// No concurrent access tests: summarize runs once per first look on the
// database's own worker.

type countingChatter struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (c *countingChatter) Chat(context.Context, string, string, int) (string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.reply, 1, nil
}

type flSummaryLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *flSummaryLogs) logf(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *flSummaryLogs) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestFirstLookSummarizesADegradedOnlyReport(t *testing.T) {
	chat := &countingChatter{reply: `{"summary":"The sequence check could not run."}`}
	logs := &flSummaryLogs{}
	run := &firstLookRun{name: "app", summarizer: firstlook.NewSummarizer(chat),
		logf: logs.logf}
	run.summarize(context.Background(), firstlook.Report{ID: 1, Database: "app",
		Checks: []firstlook.Check{{Rule: firstlook.RuleSequenceRunway,
			Status: firstlook.CheckDegraded, Note: "GRANT SELECT"}}})
	if chat.calls != 1 {
		t.Fatalf("model calls = %d, want 1 for a degraded-only first look", chat.calls)
	}
	// No pool in this test: saving the summary fails and is logged, not lost.
	if !strings.Contains(logs.joined(), "save summary") {
		t.Fatalf("logs = %q, want the failed save reported", logs.joined())
	}
}

func TestFirstLookCleanEmptyReportSkipsTheModelAndTheSave(t *testing.T) {
	chat := &countingChatter{reply: `{"summary":"x"}`}
	logs := &flSummaryLogs{}
	run := &firstLookRun{name: "app", summarizer: firstlook.NewSummarizer(chat),
		logf: logs.logf}
	run.summarize(context.Background(), firstlook.Report{ID: 1, Database: "app",
		Checks: []firstlook.Check{{Rule: firstlook.RuleSequenceRunway,
			Status: firstlook.CheckOK}}})
	if chat.calls != 0 || logs.joined() != "" {
		t.Fatalf("clean empty report: calls %d logs %q, want neither", chat.calls,
			logs.joined())
	}
	(&firstLookRun{name: "app", logf: logs.logf}).summarize(context.Background(),
		firstlook.Report{})
	if logs.joined() != "" {
		t.Fatalf("no summarizer logged %q", logs.joined())
	}
}
