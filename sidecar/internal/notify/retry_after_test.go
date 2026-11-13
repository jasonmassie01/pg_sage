package notify

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// market-14: a 429 or 503 with Retry-After is retried by the outbox no
// sooner than the destination asked, and never later than an hour.

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":      0,
		"0":     0,
		"120":   120 * time.Second,
		" 7 ":   7 * time.Second,
		"-5":    0,
		"abc":   0,
		"1e9":   0,
		"99999": 99999 * time.Second,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRetryDelay_HonoursRetryAfterWithinBounds(t *testing.T) {
	base := outboxBackoff(1)
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"no hint", &HTTPStatusError{Code: 503}, base},
		{"shorter hint keeps the backoff",
			&HTTPStatusError{Code: 429, RetryAfter: time.Second}, base},
		{"longer hint wins", &HTTPStatusError{Code: 429, RetryAfter: 10 * time.Minute},
			10 * time.Minute},
		{"hint is capped", &HTTPStatusError{Code: 429, RetryAfter: 30 * time.Hour},
			outboxMaxRetryAfter},
		{"network error", context.DeadlineExceeded, base},
	}
	for _, tc := range cases {
		if got := retryDelay(tc.err, 1); got != tc.want {
			t.Errorf("%s: retryDelay = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOutbox_429WithRetryAfterWaitsThenDeliversOnce(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	fake, srv := newDestFake(t, http.StatusTooManyRequests)
	fake.set(http.StatusTooManyRequests, "600")
	id := rig.store.AddChannel(Channel{Name: "teams", Type: "teams",
		Config: map[string]string{"webhook_url": srv.URL}, Enabled: true})
	rig.store.AddRule(Rule{ChannelID: id, Event: "finding_opened", MinSeverity: "info",
		Enabled: true})
	res, err := rig.d.Submit(context.Background(), findingEvt("critical"))
	if err != nil || res.Enqueued != 1 {
		t.Fatalf("Submit = %+v, %v; want enqueued=1", res, err)
	}
	if got := rig.drain(t); got.Retried != 1 {
		t.Fatalf("first drain = %+v, want one retry", got)
	}
	fake.set(http.StatusAccepted, "")
	rig.clock.Advance(outboxBackoff(1))
	if got := rig.drain(t); got.Sent != 0 {
		t.Fatalf("drain after the 30 s backoff = %+v; Retry-After 600 not honoured", got)
	}
	rig.clock.Advance(600 * time.Second)
	if got := rig.drain(t); got.Sent != 1 {
		t.Fatalf("drain after Retry-After = %+v, want sent", got)
	}
	if n := len(fake.requests()); n != 2 {
		t.Fatalf("teams hits = %d, want 2 (429, then 202)", n)
	}
}
