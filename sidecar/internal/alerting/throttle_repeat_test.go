package alerting

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// alerting-07: a persistent finding is alerted once, then reminded at
// alerting.repeat_minutes (critical 240, warning 1440, info 0 = never),
// not every cooldown. The cooldown stays as flap protection between a
// resolve and a reopen (a new finding row, i.e. a new episode).

// repeatClock is a fake throttle clock safe for concurrent readers.
type repeatClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *repeatClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *repeatClock) set(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = repeatEpoch.Add(d)
}

// repeatEpoch is 09:00 UTC, outside every quiet window the tests use.
var repeatEpoch = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// repeatThrottle builds a throttle at the default cooldown with repeat
// overrides (nil keeps the defaults) on a fake clock at repeatEpoch.
func repeatThrottle(t *testing.T, repeat map[string]int) (*Throttle, *repeatClock) {
	t.Helper()
	th := NewThrottle(config.DefaultAlertingCooldown, "", "", "UTC")
	if repeat != nil {
		th.setRepeat(repeat)
	}
	clock := &repeatClock{at: repeatEpoch}
	th.now = clock.now
	return th, clock
}

// cycle is one dispatch of key: decide, then record what was delivered.
func cycle(th *Throttle, key, sev string, episode int64) (Decision, time.Duration) {
	d, openFor := th.DecideEpisode(key, sev, episode)
	if d == DecisionSend || d == DecisionRemind {
		th.RecordEpisode(key, sev, episode)
	}
	return d, openFor
}

func wantDecision(t *testing.T, label string, got, want Decision) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: decision = %v, want %v", label, got, want)
	}
}

func TestRepeat_OpenSentThrottledThenOneReminder(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	d, openFor := cycle(th, "k", "critical", 1)
	wantDecision(t, "first observation", d, DecisionSend)
	if openFor != 0 {
		t.Errorf("first alert openFor = %v, want 0", openFor)
	}
	for i := 1; i <= 10; i++ {
		clock.set(time.Duration(i) * 20 * time.Minute)
		d, _ := cycle(th, "k", "critical", 1)
		wantDecision(t, fmt.Sprintf("cycle at %dm", i*20), d, DecisionThrottled)
	}
	clock.set(240 * time.Minute)
	d, openFor = cycle(th, "k", "critical", 1)
	wantDecision(t, "at 240m", d, DecisionRemind)
	if openFor != 240*time.Minute {
		t.Errorf("reminder openFor = %v, want 4h0m0s", openFor)
	}
	clock.set(250 * time.Minute)
	d, _ = cycle(th, "k", "critical", 1)
	wantDecision(t, "10m after the reminder", d, DecisionThrottled)
	if rec := th.sent["k"]; rec.reminders != 1 || !rec.first.Equal(repeatEpoch) {
		t.Errorf("record = %+v, want reminders 1 and first at the epoch", rec)
	}
}

func TestRepeat_EscalationInsideIntervalSendsAtOnce(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	wantDecision(t, "warning first", first(cycle(th, "k", "warning", 1)), DecisionSend)
	clock.set(30 * time.Minute)
	d, openFor := cycle(th, "k", "critical", 1)
	wantDecision(t, "escalation at 30m", d, DecisionSend)
	if openFor != 0 {
		t.Errorf("escalation openFor = %v, want 0 (it is not a reminder)", openFor)
	}
	clock.set(269 * time.Minute)
	wantDecision(t, "239m after the escalation",
		first(cycle(th, "k", "critical", 1)), DecisionThrottled)
	clock.set(270 * time.Minute)
	d, openFor = cycle(th, "k", "critical", 1)
	wantDecision(t, "240m after the escalation", d, DecisionRemind)
	if openFor != 270*time.Minute {
		t.Errorf("reminder openFor = %v, want 4h30m since the first alert", openFor)
	}
}

func first(d Decision, _ time.Duration) Decision { return d }

func TestRepeat_BoundaryCriticalAt240Minutes(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "k", "critical", 1)
	clock.set(239*time.Minute + 59*time.Second)
	wantDecision(t, "239m59s", first(th.DecideEpisode("k", "critical", 1)),
		DecisionThrottled)
	clock.set(240 * time.Minute)
	wantDecision(t, "240m0s", first(th.DecideEpisode("k", "critical", 1)),
		DecisionRemind)
}

func TestRepeat_BoundaryWarningDefaultIs1440Minutes(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "k", "warning", 1)
	clock.set(1439 * time.Minute)
	wantDecision(t, "1439m", first(th.DecideEpisode("k", "warning", 1)),
		DecisionThrottled)
	clock.set(1440 * time.Minute)
	wantDecision(t, "1440m", first(th.DecideEpisode("k", "warning", 1)),
		DecisionRemind)
}

// countSends drives 600 s analyzer cycles over [0, span) for one finding
// and returns the minutes of every send and every reminder.
func countSends(th *Throttle, clock *repeatClock, sev string, span time.Duration) (
	sends, reminders []int,
) {
	for at := time.Duration(0); at < span; at += 600 * time.Second {
		clock.set(at)
		switch d, _ := cycle(th, "k", sev, 1); d {
		case DecisionSend:
			sends = append(sends, int(at/time.Minute))
		case DecisionRemind:
			reminders = append(reminders, int(at/time.Minute))
		}
	}
	return sends, reminders
}

func TestRepeat_InfoWithZeroNeverRemindsOverSevenDays(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	sends, reminders := countSends(th, clock, "info", 7*24*time.Hour)
	if len(sends) != 1 || sends[0] != 0 || len(reminders) != 0 {
		t.Fatalf("info over 7 days: sends %v reminders %v, want [0] and none",
			sends, reminders)
	}
}

func TestRepeat_ZeroDisablesCriticalReminders(t *testing.T) {
	th, clock := repeatThrottle(t, map[string]int{"critical": 0})
	sends, reminders := countSends(th, clock, "critical", 48*time.Hour)
	if len(sends) != 1 || len(reminders) != 0 {
		t.Fatalf("critical with repeat 0: sends %v reminders %v, want one send",
			sends, reminders)
	}
}

// CHECK-16: one critical finding over a day of 600 s cycles is one alert
// and five reminders, not about 72 sends.
func TestRepeat_CHECK16_OneDayOfCyclesIsOneAlertAndFiveReminders(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	sends, reminders := countSends(th, clock, "critical", 24*time.Hour)
	if fmt.Sprint(sends) != "[0]" {
		t.Errorf("sends = %v, want [0]", sends)
	}
	if got := fmt.Sprint(reminders); got != "[240 480 720 960 1200]" {
		t.Errorf("reminders at minutes %s, want [240 480 720 960 1200]", got)
	}
}

func TestRepeat_MissingKeyKeepsDefault(t *testing.T) {
	th, _ := repeatThrottle(t, map[string]int{"critical": 60})
	cases := map[string]time.Duration{
		"critical": time.Hour, "warning": 1440 * time.Minute, "info": 0,
	}
	for sev, want := range cases {
		if got := th.repeatFor(sev); got != want {
			t.Errorf("repeatFor(%s) = %v, want %v", sev, got, want)
		}
	}
}

// A repeat below the cooldown never reminds faster than the cooldown;
// config refuses such a value, this guards a caller that skips config.
func TestRepeat_BelowCooldownUsesCooldown(t *testing.T) {
	th, clock := repeatThrottle(t, map[string]int{"critical": 1})
	cycle(th, "k", "critical", 1)
	clock.set(14 * time.Minute)
	wantDecision(t, "14m", first(cycle(th, "k", "critical", 1)), DecisionThrottled)
	clock.set(15 * time.Minute)
	wantDecision(t, "15m", first(cycle(th, "k", "critical", 1)), DecisionRemind)
}

func TestRepeat_NegativeRepeatNeverReminds(t *testing.T) {
	th, clock := repeatThrottle(t, map[string]int{"critical": -5})
	sends, reminders := countSends(th, clock, "critical", 12*time.Hour)
	if len(sends) != 1 || len(reminders) != 0 {
		t.Fatalf("negative repeat: sends %v reminders %v, want one send", sends, reminders)
	}
}

func TestRepeat_ReopenInsideCooldownIsThrottledThenSentAsNew(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "k", "critical", 1)
	clock.set(2 * time.Minute)
	wantDecision(t, "reopen at 2m", first(cycle(th, "k", "critical", 2)),
		DecisionThrottled)
	clock.set(15 * time.Minute)
	d, openFor := cycle(th, "k", "critical", 2)
	wantDecision(t, "reopen at 15m", d, DecisionSend)
	if openFor != 0 {
		t.Errorf("new episode openFor = %v, want 0", openFor)
	}
	clock.set(254 * time.Minute)
	wantDecision(t, "239m into episode 2", first(cycle(th, "k", "critical", 2)),
		DecisionThrottled)
	clock.set(255 * time.Minute)
	d, openFor = cycle(th, "k", "critical", 2)
	wantDecision(t, "240m into episode 2", d, DecisionRemind)
	if openFor != 240*time.Minute {
		t.Errorf("episode 2 reminder openFor = %v, want 4h0m0s", openFor)
	}
}

func TestRepeat_UnknownEpisodeMatchesTheRecord(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	th.RecordEpisode("k", "critical", 9)
	clock.set(time.Hour)
	wantDecision(t, "episode 0", first(th.DecideEpisode("k", "critical", 0)),
		DecisionThrottled)
	wantDecision(t, "Decide", th.Decide("k", "critical"), DecisionThrottled)
	if th.ShouldAlert("k", "critical") {
		t.Error("ShouldAlert true inside the repeat interval")
	}
	clock.set(4 * time.Hour)
	if !th.ShouldAlert("k", "critical") {
		t.Error("ShouldAlert false once a reminder is due")
	}
}

// Quiet hours defer only an alert that would go out; a throttled one is
// not written as deferred. A critical reminder bypasses quiet hours.
func TestRepeat_QuietHoursDeferDueReminderOnly(t *testing.T) {
	th := NewThrottle(config.DefaultAlertingCooldown, "22:00", "06:00", "UTC")
	clock := &repeatClock{at: repeatEpoch}
	th.now = clock.now
	clock.set(12 * time.Hour) // 21:00
	wantDecision(t, "warning at 21:00", first(cycle(th, "w", "warning", 1)),
		DecisionSend)
	wantDecision(t, "critical at 21:00", first(cycle(th, "c", "critical", 2)),
		DecisionSend)
	clock.set(14 * time.Hour) // 23:00, not due
	wantDecision(t, "warning not due in quiet hours",
		first(cycle(th, "w", "warning", 1)), DecisionThrottled)
	clock.set(38 * time.Hour) // 23:00 next day, due
	wantDecision(t, "warning due in quiet hours",
		first(cycle(th, "w", "warning", 1)), DecisionDefer)
	wantDecision(t, "critical due in quiet hours",
		first(cycle(th, "c", "critical", 2)), DecisionRemind)
	clock.set(45 * time.Hour) // 06:00
	wantDecision(t, "warning after quiet hours",
		first(cycle(th, "w", "warning", 1)), DecisionRemind)
}

// A record stays while its finding is still observed (so repeat 0 holds
// for days) and is evicted once nothing has asked about it for longer
// than the longest repeat or the info cooldown.
func TestRepeat_EvictionKeepsObservedRecords(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	cycle(th, "watched", "info", 1)
	cycle(th, "gone", "critical", 2)
	for h := 1; h <= 25; h++ {
		clock.set(time.Duration(h) * time.Hour)
		cycle(th, "watched", "info", 1)
	}
	th.Record("fresh", "info")
	if _, ok := th.sent["gone"]; ok {
		t.Error("record unobserved for 25h was not evicted")
	}
	if _, ok := th.sent["watched"]; !ok {
		t.Error("record observed every hour was evicted")
	}
	if n := len(th.sent); n != 2 {
		t.Errorf("sent map size = %d, want 2", n)
	}
}

func TestRepeat_EvictionWindowCoversLongestRepeat(t *testing.T) {
	th, clock := repeatThrottle(t, map[string]int{"warning": 2880})
	cycle(th, "k", "warning", 1)
	clock.set(47 * time.Hour)
	th.Record("other", "info")
	if _, ok := th.sent["k"]; !ok {
		t.Fatal("record evicted before the 48h repeat could fire")
	}
	clock.set(48 * time.Hour)
	wantDecision(t, "48h", first(th.DecideEpisode("k", "warning", 1)), DecisionRemind)
}

// Concurrent access: the dispatcher (one goroutine per Manager) decides
// and records one key while another goroutine seeds and decides other
// keys. The dispatcher still sees one alert and one reminder per interval.
func TestRepeat_ConcurrentSeedAndDecideKeepOneReminderPerInterval(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("seed%d", i%50)
			th.RecordAt(key, "warning", time.Time{})
			th.Decide(key, "warning")
		}
	}()
	sends, reminders := countSends(th, clock, "critical", 24*time.Hour)
	close(stop)
	wg.Wait()
	if len(sends) != 1 || len(reminders) != 5 {
		t.Fatalf("under concurrent seeding: sends %v reminders %v, want 1 and 5",
			sends, reminders)
	}
}

func TestRepeat_RestoreKeepsFirstAndLastSend(t *testing.T) {
	th, clock := repeatThrottle(t, nil)
	clock.set(5 * time.Hour)
	th.restore("k", "critical", 3, repeatEpoch, repeatEpoch.Add(30*time.Minute))
	d, openFor := th.DecideEpisode("k", "critical", 3)
	wantDecision(t, "4h30m after the seeded send", d, DecisionRemind)
	if openFor != 5*time.Hour {
		t.Errorf("openFor = %v, want 5h from the seeded first send", openFor)
	}
	clock.set(4 * time.Hour)
	wantDecision(t, "3h30m after the seeded send",
		first(th.DecideEpisode("k", "critical", 3)), DecisionThrottled)
}
