package briefing

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06: the schedule, channels and webhook follow a live config change.

func liveWorker(t *testing.T, schedule string) *Worker {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Briefing.Schedule = schedule
	w := New(nil, cfg, nil, noopLog)
	w.markRanAt(time.Date(2026, 11, 11, 6, 30, 0, 0, time.UTC))
	return w
}

// State transition: a new schedule changes the next ShouldRun answer.
func TestReconfigureChangesShouldRun(t *testing.T) {
	w := liveWorker(t, "0 6 * * *")
	at := time.Date(2026, 11, 11, 7, 5, 0, 0, time.UTC)
	if w.ShouldRun(at) {
		t.Fatal("06:00 schedule fired again at 07:05 after the 06:30 run")
	}
	if err := w.Reconfigure(config.BriefingConfig{Schedule: "0 7 * * *",
		Channels: []string{"slack"}, SlackWebhookURL: "http://hook"}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if !w.ShouldRun(at) {
		t.Fatal("07:00 schedule did not fire at 07:05 after a live change")
	}
	s := w.settings()
	if s.cfg.SlackWebhookURL != "http://hook" || len(s.cfg.Channels) != 1 {
		t.Fatalf("settings = %+v, want the new channels and webhook", s.cfg)
	}
}

// Invalid input: a bad schedule is refused and the worker keeps the old one.
func TestReconfigureRejectsInvalidSchedule(t *testing.T) {
	w := liveWorker(t, "0 6 * * *")
	err := w.Reconfigure(config.BriefingConfig{Schedule: "61 * * * *"})
	if err == nil || !strings.Contains(err.Error(), "minute") {
		t.Fatalf("Reconfigure(61 * * * *) = %v, want a minute error", err)
	}
	if got := w.settings().cfg.Schedule; got != "0 6 * * *" {
		t.Fatalf("schedule after a refused change = %q, want the old one", got)
	}
	if err := ValidateSchedule("61 * * * *"); err == nil {
		t.Fatal("ValidateSchedule accepted minute 61")
	}
	if err := ValidateSchedule("*/5 * * * 1-5"); err != nil {
		t.Fatalf("ValidateSchedule rejected a valid cron: %v", err)
	}
}

// Nil/empty: an empty schedule turns briefings off rather than failing.
func TestReconfigureEmptyScheduleDisables(t *testing.T) {
	w := liveWorker(t, "0 6 * * *")
	if err := w.Reconfigure(config.BriefingConfig{}); err != nil {
		t.Fatalf("Reconfigure(empty) = %v, want nil (briefings off)", err)
	}
	if w.ShouldRun(time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)) {
		t.Fatal("an empty schedule still fires")
	}
	if err := ValidateSchedule(""); err != nil {
		t.Fatalf("ValidateSchedule(\"\") = %v, want nil", err)
	}
}

// Nil/empty at startup: New with an empty schedule is off without a WARN,
// while an invalid one still warns and never fires.
func TestNewEmptyScheduleIsOffWithoutWarning(t *testing.T) {
	var logs []string
	logFn := func(level, format string, args ...any) {
		logs = append(logs, level+" "+format)
	}
	for _, schedule := range []string{"", "  "} {
		cfg := config.DefaultConfig()
		cfg.Briefing.Schedule = schedule
		w := New(nil, cfg, nil, logFn)
		if w.ShouldRun(time.Now().Add(48 * time.Hour)) {
			t.Fatalf("schedule %q fired", schedule)
		}
	}
	if len(logs) != 0 {
		t.Fatalf("an empty schedule logged %v, want nothing", logs)
	}
	cfg := config.DefaultConfig()
	cfg.Briefing.Schedule = "61 * * * *"
	New(nil, cfg, nil, logFn)
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "WARN briefing: invalid schedule") {
		t.Fatalf("an invalid schedule logged %v, want one WARN", logs)
	}
}

// Concurrent access: a reader during reconfiguration sees the old or the new
// settings whole, never a schedule from one and channels from the other.
func TestReconfigureIsNeverTorn(t *testing.T) {
	w := liveWorker(t, "0 6 * * *")
	a := config.BriefingConfig{Schedule: "0 6 * * *", Channels: []string{"stdout"}}
	b := config.BriefingConfig{Schedule: "0 7 * * *", Channels: []string{"slack"},
		SlackWebhookURL: "http://hook"}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			next := a
			if i%2 == 1 {
				next = b
			}
			if err := w.Reconfigure(next); err != nil {
				t.Errorf("Reconfigure: %v", err)
				return
			}
		}
		close(stop)
	}()
	torn := 0
	for done := false; !done; {
		select {
		case <-stop:
			done = true
		default:
		}
		s := w.settings()
		pairA := s.cfg.Schedule == a.Schedule && s.cfg.Channels[0] == "stdout" &&
			s.schedule.hours[6] && !s.schedule.hours[7]
		pairB := s.cfg.Schedule == b.Schedule && s.cfg.Channels[0] == "slack" &&
			s.schedule.hours[7] && !s.schedule.hours[6]
		if !pairA && !pairB {
			torn++
		}
	}
	wg.Wait()
	if torn != 0 {
		t.Fatalf("%d reads saw a torn schedule/channel pair", torn)
	}
}

// The worker never shares the caller's channel slice.
func TestReconfigureCopiesChannels(t *testing.T) {
	w := liveWorker(t, "0 6 * * *")
	channels := []string{"slack"}
	if err := w.Reconfigure(config.BriefingConfig{Schedule: "0 6 * * *",
		Channels: channels}); err != nil {
		t.Fatal(err)
	}
	channels[0] = "stdout"
	if got := w.settings().cfg.Channels[0]; got != "slack" {
		t.Fatalf("worker channel = %q after the caller changed its slice", got)
	}
}
