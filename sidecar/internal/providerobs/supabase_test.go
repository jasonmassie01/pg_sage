package providerobs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, status int, body string) *Supabase {
	t.Helper()
	c, err := NewSupabase("project123", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected request method/auth")
		}
		if r.URL.Host != "api.supabase.com" {
			t.Errorf("host = %s", r.URL.Host)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{}, Request: r}, nil
	})
	return c
}

func TestSupabaseLogsPreserveIdentityAndMetadata(t *testing.T) {
	body := `{"result":[{"timestamp":"2026-09-07T12:00:01Z","database":"postgres",
		"event_message":"deadlock detected","sql_state":"40P01","severity":"ERROR",
		"application":"test-client","query":"UPDATE t SET n=1","pid":"12"},
		{"timestamp":"2026-09-07T12:00:02Z","database":"other","event_message":"secret"}]}`
	c := fixtureClient(t, 200, body)
	from := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	entries, err := c.Logs(context.Background(), "postgres", from, from.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Database != "postgres" || entries[0].PID != 12 ||
		entries[0].SQLState != "40P01" || entries[0].Application != "test-client" ||
		entries[0].Query != "UPDATE t SET n=1" || entries[0].Timestamp != from.Add(time.Second) {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestSupabaseLogsRejectInvalidOrFailedResponses(t *testing.T) {
	for _, body := range []string{`{`, `{"error":"denied","result":[]}`,
		`{"result":[{"database":"postgres","timestamp":"wrong"}]}`,
		`{}`, `{"result":null}`} {
		c := fixtureClient(t, 200, body)
		_, err := c.Logs(context.Background(), "postgres", time.Now().Add(-time.Minute), time.Now())
		if err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	for _, status := range []int{301, 401, 402, 403, 429, 500} {
		c := fixtureClient(t, status, "credential-secret-response")
		_, err := c.Logs(context.Background(), "postgres", time.Now().Add(-time.Minute), time.Now())
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) ||
			strings.Contains(err.Error(), "credential-secret-response") {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

func TestSupabaseLogsWindowAndEmpty(t *testing.T) {
	c := fixtureClient(t, 200, `{"result":[]}`)
	now := time.Now().UTC().Truncate(time.Minute)
	for _, span := range []time.Duration{0, -time.Minute, 25 * time.Hour} {
		if _, err := c.Logs(context.Background(), "postgres", now, now.Add(span)); err == nil {
			t.Errorf("accepted window %s", span)
		}
	}
	entries, err := c.Logs(context.Background(), "postgres", now, now.Add(time.Minute))
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty = %#v %v", entries, err)
	}
	if _, err := c.Logs(context.Background(), "", now, now.Add(time.Minute)); err == nil {
		t.Fatal("accepted unknown database")
	}
}

func TestSupabaseBoundaryAndCancellation(t *testing.T) {
	for _, ref := range []string{"", "../other", "ref?token=x"} {
		if _, err := NewSupabase(ref, "token"); err == nil {
			t.Errorf("accepted ref %q", ref)
		}
	}
	if _, err := NewSupabase("project", ""); err == nil {
		t.Fatal("accepted empty credential")
	}
	c := fixtureClient(t, 200, strings.Repeat("x", maxResponseBytes+1))
	if _, err := c.Metrics(context.Background()); err == nil {
		t.Fatal("accepted oversized metrics")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Metrics(ctx); err == nil {
		t.Fatal("ignored cancellation")
	}
}
