package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// alerting-03: a fact card asks a human to decide, like every other
// approval card, so it must reach an approval_needed rule created with
// the default min_severity.

// loggingStore is a memStore that records delivery rows.
type loggingStore struct {
	memStore
	mu     sync.Mutex
	logged []string
}

func (s *loggingStore) LogDelivery(_ context.Context, channelID int, evt Event,
	status, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logged = append(s.logged, fmt.Sprintf("%d:%s:%s", channelID, evt.Type, status))
	return nil
}

// logLines records dispatcher log calls as "LEVEL message".
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) fn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logLines) withLevel(level string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.HasPrefix(line, level+" ") {
			out = append(out, line)
		}
	}
	return out
}

func factRuleDispatcher(minSeverity string) (*Dispatcher, *cardSender,
	*loggingStore, *logLines) {
	store := &loggingStore{memStore: memStore{
		rules: []Rule{{ID: 7, ChannelID: 3, Event: "approval_needed",
			MinSeverity: minSeverity, Enabled: true}},
		channels: map[int]*Channel{3: {ID: 3, Type: "email", Enabled: true,
			Config: map[string]string{}}},
	}}
	logs := &logLines{}
	sender := &cardSender{typ: "email"}
	d := NewDispatcherWithStore(store, logs.fn)
	d.RegisterSender(sender)
	return d, sender, store, logs
}

func TestFactCardReachesDefaultApprovalRule(t *testing.T) {
	d, sender, store, logs := factRuleDispatcher(DefaultMinSeverity("approval_needed"))
	evt := FactProposedEvent(factRef(), "Evidence: recreated 8 times")
	if err := d.Dispatch(context.Background(), evt); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(sender.events) != 1 {
		t.Fatalf("default approval_needed rule delivered %d fact cards, want 1",
			len(sender.events))
	}
	got := sender.events[0]
	if got.Severity != "warning" || !strings.HasPrefix(got.Subject, "Confirm a fact: ") {
		t.Fatalf("delivered card severity %q subject %q", got.Severity, got.Subject)
	}
	if ref, ok := FactCardOf(got); !ok || ref.FactID != 12 {
		t.Fatalf("delivered event lost its fact card: %+v", got.Data)
	}
	if len(store.logged) != 1 || store.logged[0] != "3:approval_needed:sent" {
		t.Fatalf("delivery rows = %v, want one sent row", store.logged)
	}
	if debug := logs.withLevel("DEBUG"); len(debug) != 0 {
		t.Fatalf("a delivered card logged a filter line: %v", debug)
	}
}

// A fact card is an approval request: it carries the same severity as
// the other approval_needed constructors so routing treats them alike.
func TestFactCardSeverityMatchesOtherApprovalCards(t *testing.T) {
	fact := FactProposedEvent(factRef(), "b")
	legacy := ApprovalNeededEvent("Reindex", "REINDEX", "orders", "high")
	sre := sreCardEvent()
	if fact.Severity != legacy.Severity || fact.Severity != sre.Severity {
		t.Fatalf("fact card severity %q, approval cards %q / %q",
			fact.Severity, legacy.Severity, sre.Severity)
	}
}

// Boundary: every min_severity at or below warning delivers the card;
// critical filters it, writes no delivery row (it would flood the log
// every cycle) and leaves a DEBUG line saying why.
func TestFactCardMinSeverityBoundary(t *testing.T) {
	for _, tc := range []struct {
		min       string
		delivered int
	}{{"info", 1}, {"warning", 1}, {"critical", 0}} {
		d, sender, store, logs := factRuleDispatcher(tc.min)
		if err := d.Dispatch(context.Background(),
			FactProposedEvent(factRef(), "b")); err != nil {
			t.Fatalf("min %s: dispatch: %v", tc.min, err)
		}
		if len(sender.events) != tc.delivered || len(store.logged) != tc.delivered {
			t.Fatalf("min %s: delivered %d rows %v, want %d", tc.min,
				len(sender.events), store.logged, tc.delivered)
		}
		debug := logs.withLevel("DEBUG")
		if tc.delivered == 1 {
			if len(debug) != 0 {
				t.Fatalf("min %s: unexpected filter line %v", tc.min, debug)
			}
			continue
		}
		if len(debug) != 1 {
			t.Fatalf("min %s: filter lines %v, want 1", tc.min, debug)
		}
		for _, want := range []string{"rule 7", "approval_needed", "warning",
			"critical"} {
			if !strings.Contains(debug[0], want) {
				t.Fatalf("filter line %q lacks %q", debug[0], want)
			}
		}
	}
}

// A disabled channel is not a severity filter: no DEBUG filter line.
func TestFactCardDisabledChannelLogsNoFilterLine(t *testing.T) {
	d, sender, store, logs := factRuleDispatcher("warning")
	store.channels[3].Enabled = false
	if err := d.Dispatch(context.Background(), FactProposedEvent(factRef(), "b")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(sender.events) != 0 || len(store.logged) != 0 ||
		len(logs.withLevel("DEBUG")) != 0 {
		t.Fatalf("disabled channel: events %d rows %v logs %v",
			len(sender.events), store.logged, logs.lines)
	}
}

// Concurrent dispatch of fact cards: each goroutine's card is delivered
// once and the filter path stays silent under a default rule.
func TestFactCardConcurrentDispatchUnderDefaultRule(t *testing.T) {
	d, sender, store, logs := factRuleDispatcher(DefaultMinSeverity("approval_needed"))
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			ref := factRef()
			ref.FactID = id
			errs <- d.Dispatch(context.Background(), FactProposedEvent(ref, "b"))
		}(int64(i + 1))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}
	if len(sender.events) != n || len(store.logged) != n ||
		len(logs.withLevel("DEBUG")) != 0 {
		t.Fatalf("delivered %d rows %d debug %v, want %d", len(sender.events),
			len(store.logged), logs.withLevel("DEBUG"), n)
	}
}
