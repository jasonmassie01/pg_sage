package cases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// market-14: Jira is a case link, not a channel. An operator creates one
// ticket per case; a bounded worker reads its status back and comments
// once when the case resolves. Ticket status is read-only evidence: it
// never resolves a case, approves an action or changes trust.

func newTicketService(store TicketStore, f *fakeTracker) TicketService {
	return TicketService{Store: store, Tracker: trackerOf(f)}
}

func TestTicketCreate_PostsSummaryAndCaseLink(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	svc := newTicketService(store, jira)
	got, created, err := svc.Create(context.Background(), TicketRequest{Case: ticketCase(),
		ChannelID: 4, CaseURL: "https://sage.example.com/cases?case=x", Actor: "op@example.com"})
	if err != nil || !created {
		t.Fatalf("Create = %+v, %v, %v; want a created ticket", got, created, err)
	}
	if got.ExternalKey != "OPS-7" || got.URL != "https://jira.example.com/browse/OPS-7" ||
		got.Database != "db1" || got.CaseIdentityKey != ticketCase().IdentityKey ||
		got.System != TicketSystemJira || got.CreatedBy != "op@example.com" {
		t.Fatalf("ticket = %+v", got)
	}
	if len(jira.created) != 1 {
		t.Fatalf("issues created = %d, want 1", len(jira.created))
	}
	req := jira.created[0]
	if req.Summary != "[pg_sage] db1: Table t is bloated" ||
		req.CaseURL != "https://sage.example.com/cases?case=x" {
		t.Fatalf("issue request = %+v", req)
	}
	for _, want := range []string{"dead tuples are 60%", "grew 10% this week", "warning",
		ticketCase().IdentityKey} {
		if !strings.Contains(req.Description, want) {
			t.Errorf("description lacks %q: %s", want, req.Description)
		}
	}
}

func TestTicketCreate_SecondCreateReturnsTheLinkedTicket(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	svc := newTicketService(store, jira)
	first, _, err := svc.Create(context.Background(),
		TicketRequest{Case: ticketCase(), ChannelID: 4})
	if err != nil {
		t.Fatal(err)
	}
	again, created, err := svc.Create(context.Background(), TicketRequest{Case: ticketCase(),
		ChannelID: 4})
	if err != nil || created || again.ID != first.ID || again.ExternalKey != "OPS-7" {
		t.Fatalf("second Create = %+v, %v, %v; want the first ticket, not created", again,
			created, err)
	}
	if len(jira.created) != 1 {
		t.Fatalf("issues created = %d, want 1", len(jira.created))
	}
}

func TestTicketCreate_ConcurrentClicksMakeOneIssue(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	svc := newTicketService(store, jira)
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := svc.Create(context.Background(),
				TicketRequest{Case: ticketCase(), ChannelID: 4})
			if err != nil && !errors.Is(err, ErrTicketInProgress) {
				t.Errorf("Create: %v", err)
			}
			if created {
				mu.Lock()
				createdCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if createdCount != 1 || len(jira.created) != 1 {
		t.Fatalf("created=%d issues=%d, want exactly one", createdCount, len(jira.created))
	}
}

func TestTicketCreate_TrackerFailureReleasesTheClaim(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{createErr: ErrTicketUnauthorized}
	svc := newTicketService(store, jira)
	_, created, err := svc.Create(context.Background(),
		TicketRequest{Case: ticketCase(), ChannelID: 4})
	if !errors.Is(err, ErrTicketUnauthorized) || created {
		t.Fatalf("Create err = %v created=%v, want ErrTicketUnauthorized", err, created)
	}
	if rows, _ := store.List(context.Background(), "all"); len(rows) != 0 {
		t.Fatalf("claim kept after a failed create: %+v", rows)
	}
	jira.createErr = nil
	if _, created, err := svc.Create(context.Background(), TicketRequest{Case: ticketCase(),
		ChannelID: 4}); err != nil || !created {
		t.Fatalf("retry after the failure = %v, %v; want created", created, err)
	}
}

func TestTicketCreate_InvalidRequests(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	svc := newTicketService(store, jira)
	noKey := ticketCase()
	noKey.IdentityKey = ""
	noDB := ticketCase()
	noDB.DatabaseName = ""
	for name, req := range map[string]TicketRequest{
		"no identity key": {Case: noKey, ChannelID: 4},
		"no database":     {Case: noDB, ChannelID: 4},
		"no channel":      {Case: ticketCase()},
		"zero value":      {},
	} {
		if _, _, err := svc.Create(context.Background(), req); !errors.Is(err, ErrTicketInvalid) {
			t.Errorf("%s: err = %v, want ErrTicketInvalid", name, err)
		}
	}
	missing := TicketService{Store: store, Tracker: func(context.Context, int) (TicketTracker,
		error) {
		return nil, ErrTicketIntegrationMissing
	}}
	_, _, err := missing.Create(context.Background(),
		TicketRequest{Case: ticketCase(), ChannelID: 9})
	if !errors.Is(err, ErrTicketIntegrationMissing) {
		t.Fatalf("missing integration err = %v", err)
	}
	if len(jira.created) != 0 {
		t.Fatal("an invalid request reached the ticket system")
	}
}

func TestTicketCreate_StoreErrorPropagates(t *testing.T) {
	store := newMemTicketStore()
	store.err = errors.New("connection refused")
	svc := newTicketService(store, &fakeTracker{})
	_, _, err := svc.Create(context.Background(), TicketRequest{Case: ticketCase(), ChannelID: 4})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the store error", err)
	}
}

func TestTicketSummary_IsBoundedOnARune(t *testing.T) {
	c := ticketCase()
	c.Title = strings.Repeat("é", 400)
	s := ticketSummary(c)
	if len(s) > jiraMaxSummary || !strings.HasPrefix(s, "[pg_sage] db1: ") ||
		!strings.HasSuffix(s, "é") {
		t.Fatalf("summary = %d bytes %q..., want at most %d on a rune", len(s), s[:20],
			jiraMaxSummary)
	}
	if got := ticketSummary(Case{DatabaseName: "d", Title: fmt.Sprint("x")}); got !=
		"[pg_sage] d: x" {
		t.Fatalf("short summary = %q", got)
	}
}
