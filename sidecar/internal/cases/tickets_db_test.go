package cases

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (market-14): sage.case_tickets on real PostgreSQL. The
// unique (database, case_identity_key, system) key gives one ticket per
// case even when two operators click at once.

func ticketPool(t *testing.T) (*pgxpool.Pool, int) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "case_tickets")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for run := 1; run <= 2; run++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("Bootstrap run %d: %v", run, err)
		}
	}
	var ch int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type)
		VALUES ('jira', 'jira') RETURNING id`).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	return pool, ch
}

func TestTicketStoreDB_ConcurrentCreateMakesOneTicket(t *testing.T) {
	pool, ch := ticketPool(t)
	store := NewTicketStore(pool)
	var issues atomic.Int32
	svc := TicketService{Store: store, Tracker: func(context.Context, int) (TicketTracker,
		error) {
		return countingTracker{n: &issues}, nil
	}}
	var wg sync.WaitGroup
	var created atomic.Int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := svc.Create(context.Background(),
				TicketRequest{Case: ticketCase(), ChannelID: ch, Actor: "op"})
			if err != nil && err != ErrTicketInProgress {
				t.Errorf("Create: %v", err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	rows, err := store.List(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Load() != 1 || issues.Load() != 1 || len(rows) != 1 ||
		rows[0].ExternalKey != "OPS-7" || rows[0].CreatedBy != "op" {
		t.Fatalf("created=%d issues=%d rows=%+v, want one ticket", created.Load(),
			issues.Load(), rows)
	}
}

type countingTracker struct{ n *atomic.Int32 }

func (c countingTracker) CreateIssue(context.Context, IssueRequest) (IssueRef, error) {
	c.n.Add(1)
	time.Sleep(50 * time.Millisecond) // widen the race window
	return IssueRef{Key: "OPS-7", URL: "https://jira.example.com/browse/OPS-7"}, nil
}
func (c countingTracker) IssueStatus(context.Context, string) (IssueStatus, error) {
	return IssueStatus{}, nil
}
func (c countingTracker) AddComment(context.Context, string, string) error { return nil }

func TestTicketStoreDB_NoTicketListsEmpty(t *testing.T) {
	pool, _ := ticketPool(t)
	rows, err := NewTicketStore(pool).List(context.Background(), "db1")
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("List = %#v, %v; want an empty, non-nil list", rows, err)
	}
}

func TestTicketStoreDB_SyncLifecycle(t *testing.T) {
	pool, ch := ticketPool(t)
	ctx := context.Background()
	store := NewTicketStore(pool)
	tk, claimed, err := store.Claim(ctx, TicketClaim{Database: "db1",
		CaseIdentityKey: "finding:db1:x", System: TicketSystemJira, ChannelID: ch})
	if err != nil || !claimed || tk.Status != "creating" {
		t.Fatalf("Claim = %+v, %v, %v", tk, claimed, err)
	}
	if due, _ := store.DueForSync(ctx, 50, 0); len(due) != 0 {
		t.Fatalf("a ticket still being created is due for sync: %+v", due)
	}
	if _, err := store.Complete(ctx, tk.ID, IssueRef{Key: "OPS-1", URL: "u"}); err != nil {
		t.Fatal(err)
	}
	due, err := store.DueForSync(ctx, 50, 10*time.Minute)
	if err != nil || len(due) != 1 || due[0].ExternalKey != "OPS-1" {
		t.Fatalf("DueForSync = %+v, %v", due, err)
	}
	if again, _ := store.DueForSync(ctx, 50, 10*time.Minute); len(again) != 0 {
		t.Fatalf("a ticket synced just now is due again: %+v", again)
	}
	for _, st := range []IssueStatus{{"Open", "new"}, {"In Progress", "indeterminate"},
		{"Done", "done"}} {
		if err := store.RecordSync(ctx, tk.ID, st, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordSync(ctx, tk.ID, IssueStatus{}, "unauthorized"); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.List(ctx, "all")
	if rows[0].Status != "Done" || rows[0].StatusCategory != "done" ||
		rows[0].SyncError != "unauthorized" || rows[0].LastSyncedAt == nil {
		t.Fatalf("ticket = %+v, want Done kept with sync_error unauthorized", rows[0])
	}
	assertCommentClaimOnce(t, store, tk.ID)
}

func assertCommentClaimOnce(t *testing.T, store *PgTicketStore, id int64) {
	t.Helper()
	ctx := context.Background()
	first, err := store.ClaimResolvedComment(ctx, id)
	second, _ := store.ClaimResolvedComment(ctx, id)
	if err != nil || !first || second {
		t.Fatalf("comment claims = %v, %v (%v); want true then false", first, second, err)
	}
	if err := store.ReleaseResolvedComment(ctx, id); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.ClaimResolvedComment(ctx, id); !again {
		t.Fatal("a released comment claim cannot be taken again")
	}
}

func TestTicketStoreDB_ReleaseAndStaleClaim(t *testing.T) {
	pool, ch := ticketPool(t)
	ctx := context.Background()
	store := NewTicketStore(pool)
	claim := TicketClaim{Database: "db2", CaseIdentityKey: "k", System: TicketSystemJira,
		ChannelID: ch}
	tk, _, _ := store.Claim(ctx, claim)
	if _, claimed, _ := store.Claim(ctx, claim); claimed {
		t.Fatal("a fresh claim was taken twice")
	}
	if err := store.Release(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	tk, claimed, err := store.Claim(ctx, claim)
	if err != nil || !claimed {
		t.Fatalf("claim after release = %v, %v", claimed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.case_tickets SET created_at = now() - interval
		'10 minutes' WHERE id = $1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if stale, claimed, err := store.Claim(ctx, claim); err != nil || !claimed || stale.ID != tk.ID {
		t.Fatalf("stale claim = %+v, %v, %v; want the abandoned row taken over", stale,
			claimed, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.case_tickets
		(database, case_identity_key, system) VALUES ('d', 'k', 'servicenow')`); err == nil {
		t.Fatal("an unknown ticket system was stored")
	}
}

func TestTicketStoreDB_ChannelDeleteKeepsTheTicket(t *testing.T) {
	pool, ch := ticketPool(t)
	ctx := context.Background()
	store := NewTicketStore(pool)
	tk, _, _ := store.Claim(ctx, TicketClaim{Database: "db1", CaseIdentityKey: "k",
		System: TicketSystemJira, ChannelID: ch})
	if _, err := store.Complete(ctx, tk.ID, IssueRef{Key: "OPS-2", URL: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.notification_channels WHERE id = $1`,
		ch); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.List(ctx, "db1")
	if len(rows) != 1 || rows[0].ChannelID != nil || rows[0].ExternalKey != "OPS-2" {
		t.Fatalf("rows = %+v, want the ticket kept with no integration", rows)
	}
}
