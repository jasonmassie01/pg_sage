package cases

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// market-14: the ticket sync worker reads issue status back, at most
// TicketSyncLimit tickets per cycle, and comments once when the case is
// no longer open. It only reads the case state.

// caseStates is a scripted, read-only case lookup that counts its calls.
type caseStates struct {
	mu    sync.Mutex
	state State
	known bool
	calls int
}

func (c *caseStates) lookup(context.Context, string, string) (State, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.state, c.known
}

func linkedTicket(t *testing.T, store *memTicketStore, jira *fakeTracker) Ticket {
	t.Helper()
	tk, _, err := newTicketService(store, jira).Create(context.Background(),
		TicketRequest{Case: ticketCase(), ChannelID: 4})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func newSyncer(store TicketStore, jira *fakeTracker, cs *caseStates) *TicketSyncer {
	return &TicketSyncer{Store: store, Tracker: trackerOf(jira), CaseState: cs.lookup}
}

func TestTicketSync_StatusMovesAndTheCaseIsUntouched(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	tk := linkedTicket(t, store, jira)
	cs := &caseStates{state: StateOpen, known: true}
	syncer := newSyncer(store, jira, cs)
	steps := []IssueStatus{{"Open", "new"}, {"In Progress", "indeterminate"}, {"Done", "done"}}
	for _, st := range steps {
		jira.set(st, nil)
		rep, err := syncer.SyncOnce(context.Background())
		if err != nil || rep.Checked != 1 || rep.Updated != 1 || rep.Failed != 0 {
			t.Fatalf("SyncOnce(%v) = %+v, %v", st, rep, err)
		}
		got := store.get(tk.ID)
		if got.Status != st.Name || got.StatusCategory != st.Category || got.SyncError != "" {
			t.Fatalf("ticket after %v = %+v", st, got)
		}
	}
	if len(jira.comments) != 0 {
		t.Fatalf("comments = %v; an open case must not be commented", jira.comments)
	}
	if cs.state != StateOpen || cs.calls != len(steps) {
		t.Fatalf("case state = %s after %d lookups; a done ticket must not resolve the case",
			cs.state, cs.calls)
	}
}

func TestTicketSync_401ShowsUnauthorizedAndKeepsStatus(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	tk := linkedTicket(t, store, jira)
	cs := &caseStates{state: StateResolved, known: true}
	syncer := newSyncer(store, jira, cs)
	jira.set(IssueStatus{"In Progress", "indeterminate"}, nil)
	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	jira.comments = nil
	_ = store.ReleaseResolvedComment(context.Background(), tk.ID)
	jira.set(IssueStatus{}, ErrTicketUnauthorized)
	rep, err := syncer.SyncOnce(context.Background())
	if err != nil || rep.Failed != 1 || rep.Updated != 0 {
		t.Fatalf("SyncOnce = %+v, %v; want one failure", rep, err)
	}
	got := store.get(tk.ID)
	if got.SyncError != "unauthorized" || got.Status != "In Progress" {
		t.Fatalf("ticket = %+v, want sync_error unauthorized and the last status kept", got)
	}
	if len(jira.comments) != 0 {
		t.Fatal("commented while the ticket system refused the credentials")
	}
}

func TestTicketSync_ErrorClasses(t *testing.T) {
	cases := map[string]error{
		"unauthorized": ErrTicketUnauthorized,
		"not_found":    ErrTicketNotFound,
		"unreachable":  errors.New("dial tcp: i/o timeout"),
	}
	for want, err := range cases {
		if got := ticketSyncError(err); got != want {
			t.Errorf("ticketSyncError(%v) = %q, want %q", err, got, want)
		}
	}
	if got := ticketSyncError(ErrTicketIntegrationMissing); got != "integration_missing" {
		t.Errorf("missing integration class = %q", got)
	}
}

func TestTicketSync_MissingIntegrationIsRecorded(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	tk := linkedTicket(t, store, jira)
	syncer := &TicketSyncer{Store: store, CaseState: (&caseStates{}).lookup,
		Tracker: func(context.Context, int) (TicketTracker, error) {
			return nil, ErrTicketIntegrationMissing
		}}
	rep, err := syncer.SyncOnce(context.Background())
	if err != nil || rep.Failed != 1 {
		t.Fatalf("SyncOnce = %+v, %v", rep, err)
	}
	if got := store.get(tk.ID); got.SyncError != "integration_missing" {
		t.Fatalf("sync_error = %q, want integration_missing", got.SyncError)
	}
}

func TestTicketSync_ResolvedCaseIsCommentedOnce(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	tk := linkedTicket(t, store, jira)
	cs := &caseStates{state: StateResolved, known: true}
	syncer := newSyncer(store, jira, cs)
	jira.set(IssueStatus{"Open", "new"}, nil)
	for i := 0; i < 3; i++ {
		if _, err := syncer.SyncOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(jira.comments) != 1 || !strings.HasPrefix(jira.comments[0], "OPS-7: ") ||
		!strings.Contains(jira.comments[0], "no longer open") {
		t.Fatalf("comments = %v, want one resolve comment on OPS-7", jira.comments)
	}
	if got := store.get(tk.ID); got.ResolvedCommentAt == nil {
		t.Fatal("resolved comment not recorded")
	}
}

func TestTicketSync_CommentFailureIsRetriedNextCycle(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	tk := linkedTicket(t, store, jira)
	syncer := newSyncer(store, jira, &caseStates{state: StateExpired, known: true})
	jira.set(IssueStatus{"Open", "new"}, nil)
	jira.commentOK = errors.New("503")
	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.get(tk.ID); got.ResolvedCommentAt != nil {
		t.Fatal("a failed comment was recorded as made")
	}
	jira.commentOK = nil
	if rep, _ := syncer.SyncOnce(context.Background()); rep.Commented != 1 {
		t.Fatalf("second cycle = %+v, want the comment", rep)
	}
}

func TestTicketSync_UnknownCaseStateIsNotCommented(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	linkedTicket(t, store, jira)
	syncer := newSyncer(store, jira, &caseStates{state: StateResolved, known: false})
	jira.set(IssueStatus{"Open", "new"}, nil)
	if _, err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(jira.comments) != 0 {
		t.Fatal("commented although the database could not be read")
	}
}

func TestTicketSync_IsBoundedAndEmptyIsQuiet(t *testing.T) {
	store, jira := newMemTicketStore(), &fakeTracker{}
	syncer := newSyncer(store, jira, &caseStates{})
	rep, err := syncer.SyncOnce(context.Background())
	if err != nil || rep != (TicketSyncReport{}) {
		t.Fatalf("empty SyncOnce = %+v, %v", rep, err)
	}
	if store.dueLimit != TicketSyncLimit || TicketSyncLimit != 50 {
		t.Fatalf("limit = %d (TicketSyncLimit %d), want 50", store.dueLimit, TicketSyncLimit)
	}
	syncer.Limit = 3
	_, _ = syncer.SyncOnce(context.Background())
	if store.dueLimit != 3 {
		t.Fatalf("limit = %d, want the configured 3", store.dueLimit)
	}
}

func TestTicketSync_StoreErrorPropagates(t *testing.T) {
	store := newMemTicketStore()
	store.err = errors.New("permission denied for table case_tickets")
	_, err := newSyncer(store, &fakeTracker{}, &caseStates{}).SyncOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the store error", err)
	}
}

func TestTicketSync_RunStopsOnCancel(t *testing.T) {
	store := newMemTicketStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		newSyncer(store, &fakeTracker{}, &caseStates{}).Run(ctx, TicketSyncInterval)
		close(done)
	}()
	cancel()
	<-done
	if store.dueLimit != TicketSyncLimit {
		t.Fatalf("Run did not sync once at start (limit seen %d)", store.dueLimit)
	}
}
