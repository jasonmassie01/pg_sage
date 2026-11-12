package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// alerting-06 M2/M3: Submit queues rule deliveries in the outbox; the
// drain delivers due entries per channel, retries retryable failures with
// backoff and applies the per (database, dedup_key, channel) state.

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// statusSeq answers the given statuses in order, then the last one.
type statusSeq struct {
	mu       sync.Mutex
	statuses []int
	hits     int
	bodies   []map[string]any
}

func (s *statusSeq) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		i := s.hits
		if i >= len(s.statuses) {
			i = len(s.statuses) - 1
		}
		s.hits++
		code := s.statuses[i]
		if code < 300 {
			s.bodies = append(s.bodies, body)
		}
		s.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *statusSeq) counts() (hits, accepted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, len(s.bodies)
}

type outboxRig struct {
	store *MemoryStore
	d     *Dispatcher
	clock *fakeClock
}

func newOutboxRig(t *testing.T, quietStart, quietEnd string) *outboxRig {
	t.Helper()
	store := NewMemoryStore()
	clock := &fakeClock{now: stateT0}
	d := NewDefaultDispatcher(store, TargetPolicy{AllowPrivate: true}, nil)
	d.now = clock.Now
	d.UseOutbox(store)
	d.SetAlertPolicy(testPolicy(t, quietStart, quietEnd))
	return &outboxRig{store: store, d: d, clock: clock}
}

func (r *outboxRig) slackChannel(url string, events ...string) int {
	id := r.store.AddChannel(Channel{Name: "slack-" + url, Type: "slack",
		Config: map[string]string{"webhook_url": url}, Enabled: true})
	for _, ev := range events {
		r.store.AddRule(Rule{ChannelID: id, Event: ev, MinSeverity: "info", Enabled: true})
	}
	return id
}

func (r *outboxRig) drain(t *testing.T) DrainResult {
	t.Helper()
	res, err := r.d.DrainOutbox(context.Background())
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	return res
}

func TestOutbox_RetriesSlack503TwiceThenDeliversOnce(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{503, 503, 200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")

	res, err := rig.d.Submit(context.Background(), findingEvt("critical"))
	if err != nil || res.Enqueued != 1 || res.Refused != 0 {
		t.Fatalf("Submit = %+v, %v; want enqueued=1", res, err)
	}
	if got := rig.drain(t); got.Retried != 1 || got.Sent != 0 {
		t.Fatalf("first drain = %+v, want one retry", got)
	}
	if got := rig.drain(t); got.Sent+got.Retried != 0 {
		t.Fatalf("drain before the backoff = %+v, want nothing due", got)
	}
	rig.clock.Advance(outboxBackoff(1))
	if got := rig.drain(t); got.Retried != 1 {
		t.Fatalf("second drain = %+v, want a second retry", got)
	}
	rig.clock.Advance(outboxBackoff(2))
	if got := rig.drain(t); got.Sent != 1 {
		t.Fatalf("third drain = %+v, want sent", got)
	}
	rig.clock.Advance(time.Hour)
	rig.drain(t)
	hits, accepted := seq.counts()
	if hits != 3 || accepted != 1 {
		t.Fatalf("slack hits=%d accepted=%d, want 3 and 1", hits, accepted)
	}
	log := rig.store.Deliveries()
	if len(log) != 3 || log[0].Status != "retrying" || log[2].Status != "sent" {
		t.Fatalf("delivery log = %+v, want retrying, retrying, sent", log)
	}
	if !strings.Contains(log[0].Error, "503") {
		t.Fatalf("retry log error = %q, want the 503", log[0].Error)
	}
}

func TestOutbox_PermanentFailureIsNotRetried(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{404}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	if _, err := rig.d.Submit(context.Background(), findingEvt("warning")); err != nil {
		t.Fatal(err)
	}
	if got := rig.drain(t); got.Failed != 1 || got.Retried != 0 {
		t.Fatalf("drain = %+v, want one permanent failure", got)
	}
	// Past every backoff but before the 60m warning reminder is due.
	rig.clock.Advance(50 * time.Minute)
	rig.drain(t)
	if hits, _ := seq.counts(); hits != 1 {
		t.Fatalf("a 404 was retried: %d hits", hits)
	}
	if e := rig.store.Outbox()[0]; e.Status != "failed" || !strings.Contains(e.LastError, "404") {
		t.Fatalf("outbox entry = %+v, want failed with the 404", e)
	}
}

func TestOutbox_GivesUpAfterMaxAttempts(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{500}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	if _, err := rig.d.Submit(context.Background(), findingEvt("critical")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= outboxMaxAttempts+2; i++ {
		rig.drain(t)
		rig.clock.Advance(outboxBackoff(i))
	}
	if hits, _ := seq.counts(); hits != outboxMaxAttempts {
		t.Fatalf("hits = %d, want exactly %d attempts", hits, outboxMaxAttempts)
	}
	if e := rig.store.Outbox()[0]; e.Status != "failed" || e.Attempts != outboxMaxAttempts {
		t.Fatalf("entry = %+v, want failed after %d attempts", e, outboxMaxAttempts)
	}
	log := rig.store.Deliveries()
	if last := log[len(log)-1]; last.Status != "error" {
		t.Fatalf("last log row = %+v, want error", last)
	}
}

func TestOutboxBackoff_GrowsAndIsCapped(t *testing.T) {
	if outboxBackoff(1) != 30*time.Second || outboxBackoff(2) != time.Minute {
		t.Fatalf("backoff 1,2 = %v,%v, want 30s,1m", outboxBackoff(1), outboxBackoff(2))
	}
	if outboxBackoff(0) != 30*time.Second {
		t.Fatalf("backoff(0) = %v, want the base", outboxBackoff(0))
	}
	if outboxBackoff(40) != 15*time.Minute {
		t.Fatalf("backoff(40) = %v, want the 15m cap", outboxBackoff(40))
	}
}

func TestOutbox_QuietHoursDeliverWarningOnceAfterWindow(t *testing.T) {
	rig := newOutboxRig(t, "22:00", "06:00")
	rig.clock.now = time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	ctx := context.Background()
	if res, _ := rig.d.Submit(ctx, findingEvt("warning")); res.Enqueued != 1 {
		t.Fatalf("Submit = %+v, want enqueued", res)
	}
	rig.clock.Advance(time.Hour)
	if res, _ := rig.d.Submit(ctx, findingEvt("warning")); res.Suppressed != 1 {
		t.Fatalf("repeat in the window = %+v, want suppressed", res)
	}
	if got := rig.drain(t); got.Sent != 0 {
		t.Fatalf("delivered inside quiet hours: %+v", got)
	}
	rig.clock.now = time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	if got := rig.drain(t); got.Sent != 1 {
		t.Fatalf("drain at 06:00 = %+v, want one delivery", got)
	}
	rig.drain(t)
	if _, accepted := seq.counts(); accepted != 1 {
		t.Fatalf("accepted = %d, want exactly one", accepted)
	}
}

func TestOutbox_CriticalBypassesQuietHours(t *testing.T) {
	rig := newOutboxRig(t, "22:00", "06:00")
	rig.clock.now = time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	if _, err := rig.d.Submit(context.Background(), findingEvt("critical")); err != nil {
		t.Fatal(err)
	}
	if got := rig.drain(t); got.Sent != 1 {
		t.Fatalf("critical in quiet hours = %+v, want sent now", got)
	}
}

func TestOutbox_ReminderSentOnceAtRepeatInterval(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	if _, err := rig.d.Submit(context.Background(), findingEvt("warning")); err != nil {
		t.Fatal(err)
	}
	rig.drain(t)
	rig.clock.Advance(59 * time.Minute)
	rig.drain(t)
	rig.clock.Advance(time.Minute) // exactly the 60m repeat interval
	if got := rig.drain(t); got.Reminders != 1 {
		t.Fatalf("drain at the interval = %+v, want one reminder queued", got)
	}
	rig.drain(t)
	_, accepted := seq.counts()
	if accepted != 2 {
		t.Fatalf("accepted = %d, want the alert and one reminder", accepted)
	}
	header, _ := json.Marshal(seq.bodies[1])
	if !strings.Contains(string(header), "Still open") {
		t.Fatalf("reminder body = %s, want a Still open header", header)
	}
}

func TestOutbox_ResolveAfterDeliveryIsSentOnce(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened", "finding_resolved")
	ctx := context.Background()
	opened := findingEvt("warning")
	if _, err := rig.d.Submit(ctx, opened); err != nil {
		t.Fatal(err)
	}
	rig.drain(t)
	resolved := ResolvedEvent(opened)
	resolved.Type = "finding_resolved"
	for range 2 {
		if _, err := rig.d.Submit(ctx, resolved); err != nil {
			t.Fatal(err)
		}
	}
	rig.drain(t)
	if _, accepted := seq.counts(); accepted != 2 {
		t.Fatalf("accepted = %d, want the alert and one resolve", accepted)
	}
}

func TestOutbox_FindingResolvedWithoutStateIsSuppressed(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_resolved")
	resolved := ResolvedEvent(findingEvt("info"))
	resolved.Type = "finding_resolved"
	res, err := rig.d.Submit(context.Background(), resolved)
	if err != nil || res.Suppressed != 1 || res.Enqueued != 0 {
		t.Fatalf("Submit = %+v, %v; want suppressed (never alerted)", res, err)
	}
}

func TestOutbox_CardEventsStaySynchronous(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "approval_needed")
	evt := ApprovalNeededEvent("Create index", "CREATE INDEX ...", "db1", "moderate")
	evt.Data[DataReplyTo] = int64(7)
	res, err := rig.d.Submit(context.Background(), evt)
	if err != nil || res.Delivered != 1 || res.Enqueued != 0 {
		t.Fatalf("Submit = %+v, %v; want a synchronous delivery", res, err)
	}
}

func TestOutbox_NonFindingEventsAreQueuedUnthrottled(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "action_failed")
	evt := ActionFailedEvent("Vacuum", "VACUUM t", "db1", "lock timeout")
	for range 2 {
		if res, _ := rig.d.Submit(context.Background(), evt); res.Enqueued != 1 {
			t.Fatalf("Submit = %+v, want enqueued every time", res)
		}
	}
	if got := rig.drain(t); got.Sent != 2 {
		t.Fatalf("drain = %+v, want both sent", got)
	}
}

func TestOutbox_NoRulesAndEmptyOutbox(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	res, err := rig.d.Submit(context.Background(), findingEvt("critical"))
	if err != nil || res != (DispatchResult{}) {
		t.Fatalf("Submit with no rules = %+v, %v; want zero", res, err)
	}
	if got := rig.drain(t); got != (DrainResult{}) {
		t.Fatalf("empty drain = %+v, want zero", got)
	}
}

func TestOutbox_MissingSenderIsRefused(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	id := rig.store.AddChannel(Channel{Name: "sms", Type: "sms", Enabled: true})
	rig.store.AddRule(Rule{ChannelID: id, Event: "finding_opened", MinSeverity: "info",
		Enabled: true})
	res, err := rig.d.Submit(context.Background(), findingEvt("critical"))
	if err != nil || res.Refused != 1 || res.Enqueued != 0 {
		t.Fatalf("Submit = %+v, %v; want refused", res, err)
	}
}

func TestOutbox_StoreErrorsPropagate(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	rig.slackChannel("https://hooks.example.com/x", "finding_opened")
	rig.store.FailRules = errors.New("connection refused")
	if _, err := rig.d.Submit(context.Background(), findingEvt("critical")); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Submit err = %v, want the rules error", err)
	}
	rig.store.FailRules = nil
	rig.store.FailSchedule = errors.New("outbox insert failed")
	res, err := rig.d.Submit(context.Background(), findingEvt("critical"))
	if err != nil || res.Refused != 1 {
		t.Fatalf("Submit = %+v, %v; want the schedule failure counted as refused", res, err)
	}
	rig.store.FailSchedule = nil
	rig.store.FailClaim = errors.New("claim timeout")
	if _, err := rig.d.DrainOutbox(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "claim timeout") {
		t.Fatalf("DrainOutbox err = %v, want the claim error", err)
	}
}

func TestOutbox_DispatchKeepsItsErrorContract(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	rig.store.FailRules = errors.New("permission denied")
	err := rig.d.Dispatch(context.Background(), findingEvt("critical"))
	if err == nil || !strings.Contains(err.Error(), "loading rules") {
		t.Fatalf("Dispatch err = %v, want loading rules", err)
	}
}

func TestOutbox_TwoDatabasesSameKeyHaveIndependentState(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	rig.slackChannel(seq.server(t).URL, "finding_opened")
	var enqueued, suppressed atomic.Int32
	var wg sync.WaitGroup
	for _, db := range []string{"db1", "db2"} {
		for range 10 {
			wg.Add(1)
			go func(db string) {
				defer wg.Done()
				evt := findingEvt("warning")
				evt.Data = map[string]any{"database": db}
				res, err := rig.d.Submit(context.Background(), evt)
				if err != nil {
					t.Errorf("Submit %s: %v", db, err)
				}
				enqueued.Add(int32(res.Enqueued))
				suppressed.Add(int32(res.Suppressed))
			}(db)
		}
	}
	wg.Wait()
	if enqueued.Load() != 2 || suppressed.Load() != 18 {
		t.Fatalf("enqueued=%d suppressed=%d, want 2 and 18", enqueued.Load(), suppressed.Load())
	}
	rig.drain(t)
	if _, accepted := seq.counts(); accepted != 2 {
		t.Fatalf("accepted = %d, want one per database", accepted)
	}
	if n := len(rig.store.States()); n != 2 {
		t.Fatalf("states = %d, want one per database", n)
	}
}

func TestOutbox_DrainDeliversEveryChannelInOnePass(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	slow := &statusSeq{statuses: []int{503}}
	fast := &statusSeq{statuses: []int{200}}
	rig.slackChannel(slow.server(t).URL, "finding_opened")
	rig.slackChannel(fast.server(t).URL, "finding_opened")
	if res, _ := rig.d.Submit(context.Background(), findingEvt("critical")); res.Enqueued != 2 {
		t.Fatalf("Submit = %+v, want one entry per channel", res)
	}
	if got := rig.drain(t); got.Sent != 1 || got.Retried != 1 {
		t.Fatalf("drain = %+v, want one sent and one retried", got)
	}
}
