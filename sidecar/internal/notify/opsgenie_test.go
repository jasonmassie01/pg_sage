package notify

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// market-14: Opsgenie Alert API v2. The alias is the dedup key, so an
// open and a resolve of one alert name the same Opsgenie alert.
// No concurrent access tests: the sender is stateless apart from its
// http.Client, which is safe for concurrent use.

func opsgenieRig(t *testing.T, status int) (*destFake, *OpsgenieSender) {
	t.Helper()
	fake, srv := newDestFake(t, status)
	s := NewOpsgenieSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	s.apiBase = srv.URL
	return fake, s
}

func opsgenieChannel(region string) Channel {
	cfg := map[string]string{"api_key": "og-key-1"}
	if region != "" {
		cfg["region"] = region
	}
	return Channel{ID: 8, Name: "dba-opsgenie", Type: "opsgenie", Config: cfg}
}

const opsgenieGoldenOpen = `{
 "message": "Replication lag on db1",
 "alias": "finding:db1:replication:lag",
 "description": "lag is 12 minutes",
 "priority": "P1",
 "source": "pg_sage",
 "entity": "db1",
 "tags": ["pg_sage", "finding_opened", "critical"],
 "details": {"database": "db1", "category": "replication",
  "event": "finding_opened", "severity": "critical"}
}`

func TestOpsgenieSend_CreatesAlertWithGoldenPayload(t *testing.T) {
	fake, s := opsgenieRig(t, http.StatusAccepted)
	if s.Type() != "opsgenie" {
		t.Fatalf("Type = %q", s.Type())
	}
	if err := s.Send(context.Background(), opsgenieChannel(""), destEvent(false)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/v2/alerts" {
		t.Fatalf("requests = %+v, want POST /v2/alerts", reqs)
	}
	if got := reqs[0].Header.Get("Authorization"); got != "GenieKey og-key-1" {
		t.Fatalf("Authorization = %q, want GenieKey og-key-1", got)
	}
	assertJSONEqual(t, reqs[0].Body, opsgenieGoldenOpen)
}

func TestOpsgenieSend_OpenAndResolveShareOneAlias(t *testing.T) {
	fake, s := opsgenieRig(t, http.StatusAccepted)
	ch := opsgenieChannel("eu")
	for _, resolve := range []bool{false, true} {
		if err := s.Send(context.Background(), ch, destEvent(resolve)); err != nil {
			t.Fatalf("Send resolve=%v: %v", resolve, err)
		}
	}
	reqs := fake.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	alias := reqs[0].Body["alias"].(string)
	wantPath := "/v2/alerts/" + url.PathEscape(alias) + "/close"
	if reqs[1].Path != wantPath || reqs[1].Query != "identifierType=alias" {
		t.Fatalf("close = %s?%s, want %s?identifierType=alias", reqs[1].Path, reqs[1].Query,
			wantPath)
	}
	assertJSONEqual(t, reqs[1].Body,
		`{"source":"pg_sage","note":"Resolved by pg_sage: Replication lag on db1"}`)
}

func TestOpsgenieSend_LongKeyBoundedTheSameForOpenAndClose(t *testing.T) {
	fake, s := opsgenieRig(t, http.StatusAccepted)
	evt := destEvent(false)
	evt.DedupKey = strings.Repeat("k/", 400)
	_ = s.Send(context.Background(), opsgenieChannel(""), evt)
	evt.Resolve = true
	_ = s.Send(context.Background(), opsgenieChannel(""), evt)
	reqs := fake.requests()
	alias := reqs[0].Body["alias"].(string)
	if len(alias) > opsgenieMaxAlias || alias == evt.DedupKey {
		t.Fatalf("alias length %d, want bounded to %d", len(alias), opsgenieMaxAlias)
	}
	if reqs[1].Path != "/v2/alerts/"+url.PathEscape(alias)+"/close" {
		t.Fatalf("close path %q does not name the open alias", reqs[1].Path)
	}
}

func TestOpsgenieSend_MessageIsCutAt130Runes(t *testing.T) {
	fake, s := opsgenieRig(t, http.StatusAccepted)
	evt := destEvent(false)
	evt.Subject = strings.Repeat("é", 200)
	if err := s.Send(context.Background(), opsgenieChannel(""), evt); err != nil {
		t.Fatal(err)
	}
	msg := fake.requests()[0].Body["message"].(string)
	if n := len([]rune(msg)); n != opsgenieMaxMessage {
		t.Fatalf("message has %d runes, want %d", n, opsgenieMaxMessage)
	}
}

func TestOpsgenieBaseURL_Regions(t *testing.T) {
	for region, want := range map[string]string{
		"": "https://api.opsgenie.com", "us": "https://api.opsgenie.com",
		"eu": "https://api.eu.opsgenie.com", "EU": "https://api.eu.opsgenie.com",
	} {
		got, err := OpsgenieBaseURL(region)
		if err != nil || got != want {
			t.Errorf("OpsgenieBaseURL(%q) = %q, %v; want %q", region, got, err, want)
		}
	}
	if _, err := OpsgenieBaseURL("apac"); err == nil {
		t.Fatal("region apac accepted, want an error")
	}
}

func TestOpsgenieSend_BadConfigIsPermanent(t *testing.T) {
	fake, s := opsgenieRig(t, http.StatusAccepted)
	noKey := opsgenieChannel("")
	delete(noKey.Config, "api_key")
	badRegion := opsgenieChannel("mars")
	for name, ch := range map[string]Channel{"no api_key": noKey, "bad region": badRegion} {
		err := s.Send(context.Background(), ch, destEvent(false))
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("%s: err = %v, want ErrPermanent", name, err)
		}
	}
	if n := len(fake.requests()); n != 0 {
		t.Fatalf("bad config reached Opsgenie %d time(s)", n)
	}
}

func TestOpsgenieSend_StatusErrorsAreTypedAndRedacted(t *testing.T) {
	fake, s := opsgenieRig(t, http.StatusTooManyRequests)
	fake.set(http.StatusTooManyRequests, "30")
	err := s.Send(context.Background(), opsgenieChannel(""), destEvent(false))
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.Service != "opsgenie" || !Retryable(err) {
		t.Fatalf("err = %v, want a retryable opsgenie status error", err)
	}
	if strings.Contains(err.Error(), "og-key-1") {
		t.Fatalf("error leaks the api key: %v", err)
	}
	fake.set(http.StatusUnauthorized, "")
	if err := s.Send(context.Background(), opsgenieChannel(""), destEvent(false)); Retryable(err) {
		t.Fatalf("401 err = %v, want permanent", err)
	}
}

func TestOpsgenieSend_PriorityMapping(t *testing.T) {
	for sev, want := range map[string]string{"critical": "P1", "warning": "P3",
		"info": "P5", "": "P5"} {
		if got := opsgeniePriority(sev); got != want {
			t.Errorf("opsgeniePriority(%q) = %q, want %q", sev, got, want)
		}
	}
}
