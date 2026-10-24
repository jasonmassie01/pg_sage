package changefeed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Feed.List reads the bound database's changes in an explicit window
// (Change Review receipts read landing anchors and confounders around a
// past landing, not only the last hours as Recent does).

func TestFeedListReadsAnExplicitWindow(t *testing.T) {
	st, scope, ctx := liveStore(t)
	feed := NewFeed(st, "db", func(context.Context) (sre.Scope, error) { return scope, nil })
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, at := range []time.Time{now.Add(-5 * time.Hour), now.Add(-3 * time.Hour),
		now.Add(-time.Hour)} {
		if _, _, err := st.Record(ctx, scope, true, uniqueEvent(t, at)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	got, err := feed.List(ctx, Filter{Since: now.Add(-4 * time.Hour),
		Until: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || !got[0].OccurredAt.Equal(now.Add(-3*time.Hour)) {
		t.Fatalf("window read %+v, want only the event 3 h ago", got)
	}
	if _, err := feed.List(ctx, Filter{Since: now, Limit: MaxList + 1}); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("limit over the max: %v, want ErrInvalid", err)
	}
}

func TestFeedListUnboundIsUnavailable(t *testing.T) {
	var feed *Feed
	if _, err := feed.List(context.Background(), Filter{}); !errors.Is(err,
		sre.ErrMetadataUnavailable) {
		t.Fatalf("nil feed: %v, want ErrMetadataUnavailable", err)
	}
}

func uniqueEvent(t *testing.T, at time.Time) Event {
	t.Helper()
	e, err := uniqueSubmission(at).Event(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}
