package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// market-14: "Create ticket" on a case. The handler never echoes a raw
// ticket-system error (it can carry the target's answer), and a ticket
// integration is only ever a jira channel that is enabled.
// No concurrent access tests here: the one-ticket guarantee is the
// table's unique key, covered in internal/cases/tickets_db_test.go.

func TestDecodeCaseTicketRequest(t *testing.T) {
	good := `{"database":"db1","case_id":"finding:db1:bloat:table:t",` +
		`"case_url":"https://sage.example.com/cases?case=x"}`
	req, err := decodeCaseTicketRequest(strings.NewReader(good))
	if err != nil || req.Database != "db1" || req.CaseID != "finding:db1:bloat:table:t" ||
		req.ChannelID != 0 {
		t.Fatalf("decode = %+v, %v", req, err)
	}
	bad := map[string]string{
		"not json":       `{`,
		"no database":    `{"case_id":"finding:db1:x"}`,
		"all databases":  `{"database":"all","case_id":"finding:db1:x"}`,
		"bad case id":    `{"database":"db1","case_id":"nope"}`,
		"no case id":     `{"database":"db1"}`,
		"js case url":    `{"database":"db1","case_id":"finding:db1:x","case_url":"javascript:x"}`,
		"negative chan":  `{"database":"db1","case_id":"finding:db1:x","channel_id":-1}`,
		"unknown fields": `{"database":"db1","case_id":"finding:db1:x","resolve":true}`,
	}
	for name, body := range bad {
		if _, err := decodeCaseTicketRequest(strings.NewReader(body)); !errors.Is(err,
			cases.ErrTicketInvalid) {
			t.Errorf("%s: err = %v, want ErrTicketInvalid", name, err)
		}
	}
}

func TestCaseTicketErrorStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
		body   string
	}{
		{cases.ErrTicketInProgress, http.StatusConflict, "being created"},
		{cases.ErrTicketIntegrationMissing, http.StatusConflict, "Jira integration"},
		{fmt.Errorf("x: %w", cases.ErrTicketInvalid), http.StatusBadRequest, "invalid"},
		{fmt.Errorf("creating: %w", cases.ErrTicketUnauthorized), http.StatusBadGateway,
			"ticket_sync: unauthorized"},
		{errors.New("dial tcp 10.0.0.5:443: secret-ish detail"), http.StatusBadGateway,
			"could not create the Jira issue"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeCaseTicketError(rec, tc.err)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%v: %d %s, want %d containing %q", tc.err, rec.Code, rec.Body, tc.status,
				tc.body)
		}
		if strings.Contains(rec.Body.String(), "10.0.0.5") {
			t.Errorf("raw error echoed: %s", rec.Body)
		}
	}
}

type fakeChannels map[int]*notify.Channel

func (f fakeChannels) GetChannel(_ context.Context, id int) (*notify.Channel, error) {
	if ch, ok := f[id]; ok {
		return ch, nil
	}
	return nil, fmt.Errorf("%w: channel %d", store.ErrNotFound, id)
}

func jiraChannel(id int, enabled bool) *notify.Channel {
	return &notify.Channel{ID: id, Type: "jira", Enabled: enabled, Config: map[string]string{
		"base_url": "https://acme.atlassian.net", "email": "e", "api_token": "t",
		"project_key": "OPS"}}
}

func TestJiraTrackerFor(t *testing.T) {
	chans := fakeChannels{1: jiraChannel(1, true), 2: jiraChannel(2, false),
		3: {ID: 3, Type: "slack", Enabled: true}}
	open := jiraTrackerFor(chans, notify.TargetPolicy{})
	if tr, err := open(context.Background(), 1); err != nil || tr == nil {
		t.Fatalf("enabled jira = %v, %v", tr, err)
	}
	for _, id := range []int{2, 3, 99} {
		if _, err := open(context.Background(), id); !errors.Is(err,
			cases.ErrTicketIntegrationMissing) {
			t.Errorf("channel %d: err = %v, want ErrTicketIntegrationMissing", id, err)
		}
	}
	broken := fakeChannels{4: {ID: 4, Type: "jira", Enabled: true,
		Config: map[string]string{"base_url": "https://acme.atlassian.net"}}}
	if _, err := jiraTrackerFor(broken, notify.TargetPolicy{})(context.Background(),
		4); !errors.Is(err, cases.ErrTicketInvalid) {
		t.Fatalf("incomplete jira config: err = %v, want ErrTicketInvalid", err)
	}
}

func TestPickJiraChannel(t *testing.T) {
	listing := func(chs ...*notify.Channel) []store.ChannelListing {
		out := make([]store.ChannelListing, 0, len(chs))
		for _, ch := range chs {
			out = append(out, store.ChannelListing{Channel: *ch})
		}
		return out
	}
	slack := &notify.Channel{ID: 3, Type: "slack", Enabled: true}
	if id, err := pickJiraChannel(listing(slack, jiraChannel(5, true),
		jiraChannel(6, false))); err != nil || id != 5 {
		t.Fatalf("one enabled jira = %d, %v; want 5", id, err)
	}
	if _, err := pickJiraChannel(listing(slack)); !errors.Is(err,
		cases.ErrTicketIntegrationMissing) {
		t.Fatalf("no jira: err = %v", err)
	}
	if _, err := pickJiraChannel(nil); !errors.Is(err, cases.ErrTicketIntegrationMissing) {
		t.Fatalf("nil listing: err = %v", err)
	}
	_, err := pickJiraChannel(listing(jiraChannel(5, true), jiraChannel(7, true)))
	if !errors.Is(err, cases.ErrTicketInvalid) || !strings.Contains(err.Error(), "channel_id") {
		t.Fatalf("two jira: err = %v, want a request to name channel_id", err)
	}
}

func TestCaseStateCache_KeepsAnswersForTTLAndUnknownOnError(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	reads := 0
	var fail error
	c := newCaseStateCache(nil, time.Minute)
	c.now = func() time.Time { return now }
	c.read = func(context.Context, string, string) (cases.State, error) {
		reads++
		return cases.StateOpen, fail
	}
	for i := 0; i < 3; i++ {
		if st, known := c.lookup(context.Background(), "db1", "finding:db1:x"); !known ||
			st != cases.StateOpen {
			t.Fatalf("lookup = %s, %v", st, known)
		}
	}
	if reads != 1 {
		t.Fatalf("reads = %d within the ttl, want 1", reads)
	}
	now = now.Add(time.Minute)
	fail = errors.New("connection refused")
	if _, known := c.lookup(context.Background(), "db1", "finding:db1:x"); known {
		t.Fatal("a failed read reported a known state")
	}
	if reads != 2 {
		t.Fatalf("reads = %d after the ttl, want 2", reads)
	}
	empty := newCaseStateCache(nil, time.Minute)
	if _, known := empty.lookup(context.Background(), "db1", "finding:db1:x"); known {
		t.Fatal("no fleet reported a known state")
	}
}

type fakeLister struct {
	listing []store.ChannelListing
	err     error
}

func (f fakeLister) ListChannels(context.Context) ([]store.ChannelListing, error) {
	return f.listing, f.err
}

type fakeTicketList struct {
	tickets []cases.Ticket
	err     error
}

func (f fakeTicketList) List(context.Context, string) ([]cases.Ticket, error) {
	return f.tickets, f.err
}

func TestListCaseTicketsHandler(t *testing.T) {
	two := []store.ChannelListing{{Channel: *jiraChannel(5, true)},
		{Channel: *jiraChannel(6, true)}}
	tcs := map[string]struct {
		lister fakeLister
		want   string
	}{
		"one jira":    {fakeLister{listing: two[:1]}, `"jira_enabled":true`},
		"two jira":    {fakeLister{listing: two}, `"jira_enabled":true`},
		"none":        {fakeLister{}, `"jira_enabled":false`},
		"list failed": {fakeLister{err: errors.New("boom")}, `"jira_enabled":false`},
	}
	for name, tc := range tcs {
		h := listCaseTicketsHandler(fakeTicketList{tickets: []cases.Ticket{}}, tc.lister)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/case-tickets?database=db1", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) ||
			!strings.Contains(rec.Body.String(), `"tickets":[]`) {
			t.Errorf("%s: %d %s, want %s and an empty list", name, rec.Code, rec.Body, tc.want)
		}
	}
	h := listCaseTicketsHandler(fakeTicketList{err: errors.New("relation does not exist")},
		fakeLister{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/case-tickets", nil))
	if rec.Code != http.StatusInternalServerError ||
		strings.Contains(rec.Body.String(), "relation") {
		t.Fatalf("store failure = %d %s, want 500 without the raw error", rec.Code, rec.Body)
	}
}
