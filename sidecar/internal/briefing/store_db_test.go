package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06b integration: Store reads sage.briefings on a real PostgreSQL.

// Happy path: the latest briefing round-trips content_json unchanged, and
// the delivery status recorded after it is read back.
func TestStoreLatestRoundTripsContentJSON(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	w := New(pool, &config.Config{}, nil, noopLog).WithDatabaseName("db1")
	r, err := w.GenerateBriefing(ctx)
	if err != nil || r.ID == 0 {
		t.Fatalf("GenerateBriefing: id %d, err %v", r.ID, err)
	}
	store := NewStore(pool)
	rec, err := store.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	stored := latestBriefing(t, pool, ctx)
	if string(rec.ContentJSON) != string(stored.contentJSON) {
		t.Fatalf("content_json changed on read:\n got %s\nwant %s", rec.ContentJSON,
			stored.contentJSON)
	}
	var got Payload
	if err := json.Unmarshal(rec.ContentJSON, &got); err != nil {
		t.Fatalf("content_json is not a Payload: %v", err)
	}
	want, _ := json.Marshal(r.Payload)
	again, _ := json.Marshal(got)
	if string(want) != string(again) {
		t.Fatalf("payload changed:\n got %s\nwant %s", again, want)
	}
	if rec.ID != r.ID || rec.Mode != "executive" || rec.ContentText != r.Text ||
		rec.LLMUsed || !rec.GeneratedAt.Equal(r.Payload.Window.End) ||
		!rec.PeriodStart.Equal(r.Payload.Window.Start) || !rec.PeriodEnd.Equal(stored.end) {
		t.Fatalf("record %+v does not match the stored row %+v", rec, stored)
	}
	if rec.DeliveryStatus != nil {
		t.Fatalf("delivery_status = %s before any dispatch, want nil", rec.DeliveryStatus)
	}
	status := map[string]Delivery{"slack": {Status: DeliveryError, HTTPStatus: 500,
		Error: "HTTP 500", At: time.Now().UTC()}}
	if err := w.RecordDelivery(ctx, r.ID, status); err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	rec, err = store.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest after delivery: %v", err)
	}
	var delivery map[string]Delivery
	if err := json.Unmarshal(rec.DeliveryStatus, &delivery); err != nil ||
		delivery["slack"].HTTPStatus != 500 || delivery["slack"].Status != DeliveryError {
		t.Fatalf("delivery_status = %s (%v), want the slack 500", rec.DeliveryStatus, err)
	}
}

// Nil/empty: no briefing yet is ErrNoBriefing, and List is empty, not nil.
func TestStoreEmptyTable(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	store := NewStore(pool)
	if _, err := store.Latest(ctx); !errors.Is(err, ErrNoBriefing) {
		t.Fatalf("Latest on an empty table: %v, want ErrNoBriefing", err)
	}
	list, err := store.List(ctx, DefaultListLimit)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("List on an empty table = %v, %v; want an empty slice", list, err)
	}
}

func seedBriefingAt(t *testing.T, ctx context.Context, ago, text string) int64 {
	t.Helper()
	pool, _ := requireDB(t)
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.briefings (generated_at, period_start,
		period_end, mode, content_text, content_json, token_count)
		VALUES (now() - $1::interval, now() - $1::interval - interval '1 day',
		        now() - $1::interval, 'executive', $2, '{"total_open": 3}', 42)
		RETURNING id`, ago, text).Scan(&id)
	if err != nil {
		t.Fatalf("seed briefing %s: %v", text, err)
	}
	return id
}

// Boundary: List is newest first and stops at the limit; a limit of one
// is the latest briefing.
func TestStoreListNewestFirstWithinLimit(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	seedBriefingAt(t, ctx, "3 days", "oldest")
	seedBriefingAt(t, ctx, "1 day", "newest")
	seedBriefingAt(t, ctx, "2 days", "middle")
	store := NewStore(pool)
	two, err := store.List(ctx, 2)
	if err != nil || len(two) != 2 || two[0].ContentText != "newest" ||
		two[1].ContentText != "middle" {
		t.Fatalf("List(2) = %+v, %v; want newest, middle", two, err)
	}
	if two[0].TokenCount == nil || *two[0].TokenCount != 42 {
		t.Fatalf("token_count = %v, want 42", two[0].TokenCount)
	}
	all, err := store.List(ctx, MaxListLimit)
	if err != nil || len(all) != 3 || all[2].ContentText != "oldest" {
		t.Fatalf("List(max) = %+v, %v; want three rows ending with the oldest", all, err)
	}
	one, err := store.List(ctx, 1)
	latest, lerr := store.Latest(ctx)
	if err != nil || lerr != nil || len(one) != 1 || one[0].ID != latest.ID {
		t.Fatalf("List(1) %+v and Latest %+v differ (%v, %v)", one, latest, err, lerr)
	}
}

// Error propagation: a cancelled context is returned, not ErrNoBriefing.
func TestStoreCancelledContext(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	seedBriefingAt(t, ctx, "1 hour", "present")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := NewStore(pool).Latest(cancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Latest with a cancelled context: %v, want context.Canceled", err)
	}
}

// Concurrent access: reads racing a writer always see whole rows.
func TestStoreConcurrentReadsSeeWholeRows(t *testing.T) {
	pool, ctx := requireDB(t)
	clearBriefings(t, pool, ctx)
	seedBriefingAt(t, ctx, "2 hours", "first")
	store := NewStore(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				rec, err := store.Latest(ctx)
				if err != nil {
					errs <- err
					return
				}
				if rec.ID == 0 || !json.Valid(rec.ContentJSON) || rec.ContentText == "" {
					errs <- errors.New("torn briefing row")
					return
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		seedBriefingAt(t, ctx, "1 hour", "more")
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
}
