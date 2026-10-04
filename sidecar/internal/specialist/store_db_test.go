package specialist

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The request store against PostgreSQL (sage.specialist_requests).

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// The migration is idempotent.
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	return pool
}

func uniqueToken(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestPGStore_RecordsAndReadsBack(t *testing.T) {
	pool := livePool(t)
	st := NewPGStore(pool)
	ctx := context.Background()
	tok := uniqueToken("tok")
	end := created.Add(time.Minute)
	in := Record{Kind: KindOpen, TokenID: tok, IdentityName: "PagerDuty", Actor: "agent:pd:" +
		tok, Transport: "pagerduty", Database: "orders", InvestigationID: string(inv),
		Created: true, Match: "new", Symptom: &Symptom{Summary: "slow", Description: "d"},
		Window: &Window{Start: created, End: &end}, ExternalRef: &ExternalRef{
			System: "pagerduty", ID: "Q" + tok, URL: "https://x/y"},
		Outbound: OutboundPending}
	rec, err := st.Record(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" || rec.CreatedAt.IsZero() {
		t.Fatalf("record %+v", rec)
	}
	got, err := st.ForInvestigation(ctx, tok, "orders", string(inv))
	if err != nil || got == nil {
		t.Fatalf("for investigation: %+v %v", got, err)
	}
	if got.IdentityName != "PagerDuty" || got.Symptom.Description != "d" ||
		!got.Window.End.Equal(end) || got.ExternalRef.URL != "https://x/y" ||
		got.Outbound != OutboundPending || !got.Created || got.Match != "new" {
		t.Fatalf("round trip %+v", got)
	}
	if none, err := st.ForInvestigation(ctx, tok, "billing", string(inv)); err != nil ||
		none != nil {
		t.Fatalf("another database: %+v %v", none, err)
	}
	has, err := st.HasExternal(ctx, "pagerduty", "Q"+tok)
	if err != nil || !has {
		t.Fatalf("has external: %t %v", has, err)
	}
	recent, err := st.Recent(ctx, 500)
	if err != nil || len(recent) == 0 {
		t.Fatalf("recent: %v", err)
	}
	found := false
	for _, r := range recent {
		found = found || r.ID == rec.ID
	}
	if !found {
		t.Fatal("recent misses the record")
	}
}

func TestPGStore_LiveOpenedAndTerminal(t *testing.T) {
	pool := livePool(t)
	st := NewPGStore(pool)
	ctx := context.Background()
	tok := uniqueToken("live")
	var ids []string
	for i, kind := range []string{KindOpen, KindOpen, KindAttach} {
		rec, err := st.Record(ctx, Record{Kind: kind, TokenID: tok, IdentityName: "x",
			Actor: "agent:x:" + tok, Transport: "http", Database: "orders",
			InvestigationID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i),
			Created:         kind == KindOpen, Match: "new", Outbound: OutboundNone})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	live, err := st.LiveOpened(ctx, tok)
	if err != nil || len(live) != 2 {
		t.Fatalf("live %+v %v", live, err)
	}
	if err := st.MarkTerminal(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if live, _ = st.LiveOpened(ctx, tok); len(live) != 1 || live[0].RecordID != ids[1] {
		t.Fatalf("after terminal %+v", live)
	}
	all, err := st.LiveOpened(ctx, "")
	if err != nil || len(all) < 1 {
		t.Fatalf("all identities: %v", err)
	}
}

func TestPGStore_RefusesInvalidRows(t *testing.T) {
	st := NewPGStore(livePool(t))
	_, err := st.Record(context.Background(), Record{Kind: "approve", TokenID: "t",
		Actor: "a", Transport: "http", Database: "orders", Outbound: OutboundNone})
	if err == nil {
		t.Fatal("an unknown kind must be refused by the table")
	}
}

// seedPending records n pending result posts of tok, after clearing any
// pending rows other tests left so claim counts are exact.
func seedPending(t *testing.T, pool *pgxpool.Pool, st *PGStore, tok string, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sage.specialist_requests SET outbound = 'failed'
		WHERE outbound = 'pending'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ref := &ExternalRef{System: "pagerduty", ID: fmt.Sprintf("Q%d", i)}
		if _, err := st.Record(ctx, Record{Kind: KindOpen, TokenID: tok, Actor: "a",
			Transport: "pagerduty", Database: "orders", InvestigationID: string(inv),
			Created: true, Match: "new", Outbound: OutboundPending,
			ExternalRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
}

// claimConcurrently claims with three workers and counts claims per row.
func claimConcurrently(t *testing.T, st *PGStore, now time.Time) map[string]int {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs, err := st.ClaimOutbound(context.Background(), now, time.Minute, 10)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, r := range recs {
				seen[r.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return seen
}

func TestPGStore_OutboundClaimsAreExclusive(t *testing.T) {
	pool := livePool(t)
	st := NewPGStore(pool)
	ctx := context.Background()
	seedPending(t, pool, st, uniqueToken("out"), 6)
	now := time.Now()
	seen := claimConcurrently(t, st, now)
	if len(seen) != 6 {
		t.Fatalf("claimed %d of 6", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("record %s claimed %d times", id, n)
		}
	}
	// Leased rows are not claimed again until the lease ends.
	again, err := st.ClaimOutbound(ctx, now, time.Minute, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-claimed under lease: %d %v", len(again), err)
	}
	var one string
	for id := range seen {
		one = id
		break
	}
	if err := st.FinishOutbound(ctx, one, OutboundFailed, "pagerduty answered 503",
		now); err != nil {
		t.Fatal(err)
	}
	if err := st.RescheduleOutbound(ctx, one, now); err != nil {
		t.Fatal(err)
	}
	var state, errText string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT outbound, outbound_error, outbound_attempts
		FROM sage.specialist_requests WHERE id = $1`, one).Scan(&state, &errText,
		&attempts); err != nil {
		t.Fatal(err)
	}
	if state != OutboundFailed || errText != "pagerduty answered 503" || attempts != 1 {
		t.Fatalf("finished row %s %q %d", state, errText, attempts)
	}
}
