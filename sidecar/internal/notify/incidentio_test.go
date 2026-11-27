package notify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// market-14: incident.io HTTP alert source events. The deduplication key
// is the dedup key, so a firing and a resolved event name one alert.
// No concurrent access tests: the sender is stateless apart from its
// http.Client, which is safe for concurrent use.

func incidentIORig(t *testing.T, status int) (*destFake, *IncidentIOSender) {
	t.Helper()
	fake, srv := newDestFake(t, status)
	s := NewIncidentIOSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	s.apiBase = srv.URL
	return fake, s
}

func incidentIOChannel() Channel {
	return Channel{ID: 9, Name: "dba-incidentio", Type: "incidentio",
		Config: map[string]string{"token": "inc-token-1", "alert_source_config_id": "01HXYZ"}}
}

const incidentIOGoldenFiring = `{
 "title": "Replication lag on db1",
 "description": "lag is 12 minutes",
 "deduplication_key": "finding:db1:replication:lag",
 "status": "firing",
 "metadata": {"database": "db1", "category": "replication",
  "event": "finding_opened", "severity": "critical"}
}`

func TestIncidentIOSend_PostsGoldenFiringEvent(t *testing.T) {
	fake, s := incidentIORig(t, http.StatusAccepted)
	if s.Type() != "incidentio" {
		t.Fatalf("Type = %q", s.Type())
	}
	err := s.Send(context.Background(), incidentIOChannel(), destEvent(false))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Path != "/v2/alert_events/http/01HXYZ" ||
		reqs[0].Method != http.MethodPost {
		t.Fatalf("requests = %+v, want POST /v2/alert_events/http/01HXYZ", reqs)
	}
	if got := reqs[0].Header.Get("Authorization"); got != "Bearer inc-token-1" {
		t.Fatalf("Authorization = %q", got)
	}
	assertJSONEqual(t, reqs[0].Body, incidentIOGoldenFiring)
}

func TestIncidentIOSend_OpenAndResolveShareOneKey(t *testing.T) {
	fake, s := incidentIORig(t, http.StatusAccepted)
	for _, resolve := range []bool{false, true} {
		err := s.Send(context.Background(), incidentIOChannel(), destEvent(resolve))
		if err != nil {
			t.Fatal(err)
		}
	}
	reqs := fake.requests()
	if reqs[0].Body["deduplication_key"] != reqs[1].Body["deduplication_key"] {
		t.Fatalf("keys differ: %v vs %v", reqs[0].Body["deduplication_key"],
			reqs[1].Body["deduplication_key"])
	}
	if reqs[0].Body["status"] != "firing" || reqs[1].Body["status"] != "resolved" {
		t.Fatalf("statuses = %v, %v; want firing then resolved", reqs[0].Body["status"],
			reqs[1].Body["status"])
	}
}

func TestIncidentIOSend_BadConfigIsPermanent(t *testing.T) {
	fake, s := incidentIORig(t, http.StatusAccepted)
	cases := map[string]func(map[string]string){
		"no token":     func(c map[string]string) { delete(c, "token") },
		"no source id": func(c map[string]string) { delete(c, "alert_source_config_id") },
		"path in id":   func(c map[string]string) { c["alert_source_config_id"] = "../../x" },
		"query in id":  func(c map[string]string) { c["alert_source_config_id"] = "a?b=c" },
		"blank spaces": func(c map[string]string) { c["alert_source_config_id"] = "  " },
		"too long id": func(c map[string]string) {
			c["alert_source_config_id"] = strings.Repeat("a", 65)
		},
		"unicode in id": func(c map[string]string) { c["alert_source_config_id"] = "abcé" },
	}
	for name, mutate := range cases {
		ch := incidentIOChannel()
		mutate(ch.Config)
		err := s.Send(context.Background(), ch, destEvent(false))
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("%s: err = %v, want ErrPermanent", name, err)
		}
	}
	if n := len(fake.requests()); n != 0 {
		t.Fatalf("bad config reached incident.io %d time(s)", n)
	}
}

func TestIncidentIOSend_ServerErrorIsRetryableAndRedacted(t *testing.T) {
	_, s := incidentIORig(t, http.StatusBadGateway)
	err := s.Send(context.Background(), incidentIOChannel(), destEvent(false))
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.Code != 502 || !Retryable(err) {
		t.Fatalf("err = %v, want a retryable 502", err)
	}
	if strings.Contains(err.Error(), "inc-token-1") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestValidIncidentIOSourceID(t *testing.T) {
	for id, want := range map[string]bool{"01HXYZ": true, "abc_DEF-123": true, "": false,
		"a/b": false, "a b": false, strings.Repeat("a", 64): true} {
		if got := ValidIncidentIOSourceID(id); got != want {
			t.Errorf("ValidIncidentIOSourceID(%q) = %v, want %v", id, got, want)
		}
	}
}
