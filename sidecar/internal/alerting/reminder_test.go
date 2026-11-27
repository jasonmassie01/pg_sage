package alerting

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// alerting-07: a reminder is marked on every channel. Slack says how long
// the finding has been open, webhooks carry "reminder": true, and
// PagerDuty re-triggers with the dedup key of the open incident.

func reminderAlert() Alert {
	return Alert{
		Findings: []AlertFinding{{
			ID: 7, Category: "repeat07", Severity: "critical",
			Title: "Replication slot retaining WAL", ObjectType: "slot",
			ObjectIdentifier: "slot_a", OccurrenceCount: 21,
		}},
		Severity: "critical", Timestamp: repeatEpoch, Database: "orders",
		Reminder: true, OpenFor: 3*time.Hour + 20*time.Minute + 40*time.Second,
	}
}

func TestSlackPayload_ReminderSaysStillOpen(t *testing.T) {
	s := NewSlack("https://hooks.slack.example.com/x", nil, noop)
	body, err := s.buildPayload(reminderAlert())
	if err != nil {
		t.Fatalf("build reminder payload: %v", err)
	}
	if !strings.Contains(string(body), "Still open for 3h20m (seen 21 times)") {
		t.Errorf("reminder payload = %s, want \"Still open for 3h20m (seen 21 times)\"", body)
	}
	if !strings.Contains(string(body), "[orders]") {
		t.Errorf("reminder payload lost the database label: %s", body)
	}
	firstAlert := reminderAlert()
	firstAlert.Reminder, firstAlert.OpenFor = false, 0
	body, err = s.buildPayload(firstAlert)
	if err != nil {
		t.Fatalf("build first payload: %v", err)
	}
	if strings.Contains(string(body), "Still open for") {
		t.Errorf("first alert payload says Still open for: %s", body)
	}
	if !strings.Contains(string(body), "1 critical finding(s)") {
		t.Errorf("first alert header changed: %s", body)
	}
}

func TestFormatOpenFor(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                   "0m",
		59 * time.Second:                    "0m",
		45 * time.Minute:                    "45m",
		time.Hour:                           "1h0m",
		3*time.Hour + 20*time.Minute:        "3h20m",
		26*time.Hour + 5*time.Minute:        "1d2h",
		-time.Minute:                        "0m",
		7*24*time.Hour + 59*time.Minute + 1: "7d0h",
	}
	for d, want := range cases {
		if got := formatOpenFor(d); got != want {
			t.Errorf("formatOpenFor(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestWebhookJSON_MarksReminder(t *testing.T) {
	body, err := json.Marshal(reminderAlert())
	if err != nil {
		t.Fatalf("marshal reminder: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if got["reminder"] != true || got["open_for_seconds"] != float64(12040) {
		t.Errorf("reminder JSON = %s, want reminder true and open_for_seconds 12040", body)
	}
	if got["severity"] != "critical" || got["database"] != "orders" {
		t.Errorf("reminder JSON lost alert fields: %s", body)
	}
	plain := reminderAlert()
	plain.Reminder, plain.OpenFor = false, 0
	body, _ = json.Marshal(plain)
	if strings.Contains(string(body), "reminder") ||
		strings.Contains(string(body), "open_for_seconds") {
		t.Errorf("first alert JSON carries reminder fields: %s", body)
	}
}

func TestPagerDuty_ReminderRetriggersSameIncident(t *testing.T) {
	p := NewPagerDuty("rk", nil, noop)
	plain := reminderAlert()
	plain.Reminder, plain.OpenFor = false, 0
	var events [2]map[string]any
	for i, a := range []Alert{plain, reminderAlert()} {
		body, err := p.buildPayload(a)
		if err != nil {
			t.Fatalf("build pagerduty payload %d: %v", i, err)
		}
		if err := json.Unmarshal(body, &events[i]); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
	}
	if events[1]["event_action"] != "trigger" {
		t.Errorf("reminder action = %v, want trigger", events[1]["event_action"])
	}
	if events[0]["dedup_key"] != events[1]["dedup_key"] ||
		events[1]["dedup_key"] != "orders:repeat07:slot_a" {
		t.Errorf("dedup keys = %v / %v, want both orders:repeat07:slot_a",
			events[0]["dedup_key"], events[1]["dedup_key"])
	}
}

// managerOnClock is a pool-less Manager (alert_log writes are skipped)
// whose throttle runs on a fake clock.
func managerOnClock(t *testing.T, ch Channel) (*Manager, *repeatClock) {
	t.Helper()
	m := New(nil, ManagerConfig{}, map[string][]Channel{"critical": {ch}}, noop)
	clock := &repeatClock{at: repeatEpoch}
	m.throttle.now = clock.now
	return m, clock
}

func persistentFinding() AlertFinding {
	return AlertFinding{ID: 7, Category: "repeat07", Severity: "critical",
		ObjectIdentifier: "slot_a", OccurrenceCount: 3}
}

// Nil/zero: a ManagerConfig without RepeatMinutes takes 240/1440/0.
func TestManager_DefaultRepeatWithoutConfig(t *testing.T) {
	m := New(nil, ManagerConfig{}, nil, noop)
	want := map[string]time.Duration{
		"critical": 240 * time.Minute, "warning": 1440 * time.Minute, "info": 0,
	}
	for sev, d := range want {
		if got := m.throttle.repeatFor(sev); got != d {
			t.Errorf("repeatFor(%s) = %v, want %v", sev, got, d)
		}
	}
}

func TestManager_ConfiguredRepeatReachesThrottle(t *testing.T) {
	m := New(nil, ManagerConfig{RepeatMinutes: map[string]int{"critical": 60}}, nil, noop)
	if got := m.throttle.repeatFor("critical"); got != time.Hour {
		t.Errorf("repeatFor(critical) = %v, want 1h", got)
	}
}

func TestManager_DispatchMarksReminder(t *testing.T) {
	ch := &scriptedChannel{}
	m, clock := managerOnClock(t, ch)
	f := persistentFinding()
	ctx := context.Background()
	m.dispatchFindings(ctx, []AlertFinding{f})
	clock.set(100 * time.Minute)
	m.dispatchFindings(ctx, []AlertFinding{f})
	clock.set(240 * time.Minute)
	m.dispatchFindings(ctx, []AlertFinding{f})
	if len(ch.alerts) != 2 {
		t.Fatalf("alerts = %d, want first alert and one reminder", len(ch.alerts))
	}
	if ch.alerts[0].Reminder || ch.alerts[0].OpenFor != 0 {
		t.Errorf("first alert = %+v, want Reminder false and OpenFor 0", ch.alerts[0])
	}
	if !ch.alerts[1].Reminder || ch.alerts[1].OpenFor != 240*time.Minute {
		t.Errorf("reminder = reminder %v openFor %v, want true and 4h",
			ch.alerts[1].Reminder, ch.alerts[1].OpenFor)
	}
}

// A reminder no channel delivered is not recorded, so it is retried on
// the next cycle rather than skipped for a whole interval.
func TestManager_FailedReminderRetriedNextCycle(t *testing.T) {
	ch := &scriptedChannel{}
	m, clock := managerOnClock(t, ch)
	f := persistentFinding()
	ctx := context.Background()
	m.dispatchFindings(ctx, []AlertFinding{f})
	ch.mu.Lock()
	ch.failN = 2 // the first reminder attempt fails
	ch.mu.Unlock()
	clock.set(240 * time.Minute)
	m.dispatchFindings(ctx, []AlertFinding{f})
	clock.set(250 * time.Minute)
	m.dispatchFindings(ctx, []AlertFinding{f})
	clock.set(260 * time.Minute)
	m.dispatchFindings(ctx, []AlertFinding{f})
	if ch.calls != 3 {
		t.Fatalf("sends = %d, want first, failed reminder, retried reminder", ch.calls)
	}
	if !ch.alerts[2].Reminder || ch.alerts[2].OpenFor != 250*time.Minute {
		t.Errorf("retried reminder = reminder %v openFor %v, want true and 4h10m",
			ch.alerts[2].Reminder, ch.alerts[2].OpenFor)
	}
}
