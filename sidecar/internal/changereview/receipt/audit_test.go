package receipt

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// Gaps the post-test audit found: paging past the feed's list limit, the
// name-match fallback for statements the parser cannot read, the anchor
// rule on its own, and failures that must surface instead of reading as
// "nothing happened".

// pagedFeed serves events newest first, at most changefeed.MaxList per
// call, honoring Since and Until like the store.
type pagedFeed struct {
	events []changefeed.Event // newest first
	calls  int
}

func (p *pagedFeed) List(_ context.Context, f changefeed.Filter) ([]changefeed.Event,
	error) {
	p.calls++
	var out []changefeed.Event
	for _, e := range p.events {
		if e.OccurredAt.Before(f.Since) || e.OccurredAt.After(f.Until) {
			continue
		}
		if out = append(out, e); len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

func TestListEventsPagesPastTheListLimit(t *testing.T) {
	feed := &pagedFeed{}
	for i := 0; i < 1200; i++ {
		feed.events = append(feed.events, changefeed.Event{ID: sre.UUID(fmt.Sprint(i)),
			Kind: changefeed.KindDeploy, OccurredAt: t0.Add(-time.Duration(i) * time.Minute)})
	}
	got, err := listEvents(context.Background(), feed, t0.Add(-48*time.Hour), t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1200 || feed.calls != 3 {
		t.Fatalf("read %d events in %d calls, want 1200 in 3", len(got), feed.calls)
	}
	if !got[1199].OccurredAt.Equal(t0.Add(-1199 * time.Minute)) {
		t.Fatalf("oldest event %s", got[1199].OccurredAt)
	}
}

// A page of events all at one instant cannot be paged past: the read
// stops instead of looping.
func TestListEventsStopsWhenAPageDoesNotMove(t *testing.T) {
	feed := &pagedFeed{}
	for i := 0; i < changefeed.MaxList+10; i++ {
		feed.events = append(feed.events, changefeed.Event{ID: sre.UUID(fmt.Sprint(i)),
			OccurredAt: t0})
	}
	got, err := listEvents(context.Background(), feed, t0.Add(-time.Hour), t0)
	if err != nil || len(got) != changefeed.MaxList || feed.calls != 1 {
		t.Fatalf("read %d in %d calls (%v)", len(got), feed.calls, err)
	}
}

type errFeed struct{}

func (errFeed) List(context.Context, changefeed.Filter) ([]changefeed.Event, error) {
	return nil, errors.New("feed down")
}

func TestObserverFailuresSurface(t *testing.T) {
	w := windows
	if _, err := (Observer{}).Observe(context.Background(), decidedReview(1), w); !errors.Is(
		err, changereview.ErrInvalid) {
		t.Fatalf("no pool: %v, want ErrInvalid", err)
	}
	if _, err := (Observer{Changes: errFeed{}}).confounders(context.Background(),
		w); err == nil {
		t.Fatal("an unreadable change feed read as no confounders")
	}
	if got, err := (Observer{}).confounders(context.Background(), w); err != nil ||
		got != nil {
		t.Fatalf("no feed: %v (%v), want none known", got, err)
	}
}

func TestNamesWord(t *testing.T) {
	for text, want := range map[string]bool{"from rv_orders where": true,
		"FROM RV_ORDERS": true, "from rv_orders_2": false, "from xrv_orders": false,
		"rv_orders": true, "from $rv_orders": false} {
		if got := namesWord(text, "rv_orders"); got != want {
			t.Errorf("namesWord(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestAnchorFor(t *testing.T) {
	r := changereview.Review{ID: "r", ContentHash: "x", CreatedAt: t0}
	twin := changereview.Review{ID: "twin", ContentHash: "x", CreatedAt: t0.Add(time.Hour)}
	other := changereview.Review{ID: "other", ContentHash: "y", CreatedAt: t0.Add(time.Hour)}
	ev := func(d time.Duration) changefeed.Event {
		return changefeed.Event{EventID: d.String(), OccurredAt: t0.Add(d)}
	}
	cases := []struct {
		name     string
		events   []changefeed.Event
		complete []changereview.Review // newest first
		want     string
	}{
		{"shipped itself", []changefeed.Event{ev(time.Minute)},
			[]changereview.Review{r}, "1m0s"},
		{"shipped its twin", []changefeed.Event{ev(2 * time.Hour)},
			[]changereview.Review{twin, r}, "2h0m0s"},
		{"superseded", []changefeed.Event{ev(2 * time.Hour)},
			[]changereview.Review{other, r}, ""},
		{"first matching event wins",
			[]changefeed.Event{ev(-time.Minute), ev(30 * time.Minute), ev(2 * time.Hour)},
			[]changereview.Review{other, r}, "30m0s"},
		{"no complete review before the event", []changefeed.Event{ev(time.Minute)},
			[]changereview.Review{twin}, ""},
	}
	for _, c := range cases {
		e, ok := anchorFor(r, c.events, c.complete)
		if got := map[bool]string{true: e.EventID, false: ""}[ok]; got != c.want {
			t.Errorf("%s: anchored by %q, want %q", c.name, got, c.want)
		}
	}
}

// A catalog read that fails fails every review in the batch: none is
// treated as landed or as unmet.
func TestEvaluateAllFailsTheBatchOnAReadError(t *testing.T) {
	reader := catalogread.New(closedPool(t), catalogread.Default())
	pc := []changereview.PostCondition{{Kind: changereview.PostRelationPresent,
		Relation: "public.t"}}
	reviews := []changereview.Review{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	reviews[0].Expectations.PostConditions = pc
	reviews[2].Expectations.PostConditions = pc
	states, failed := evaluateAll(context.Background(), reader, reviews)
	if len(states) != 0 || len(failed) != 2 || failed["a"] == nil || failed["c"] == nil {
		t.Fatalf("states %v failed %v, want a and c failed", states, failed)
	}
	if s, f := evaluateAll(context.Background(), reader, reviews[1:2]); len(s)+len(f) != 0 {
		t.Fatalf("no post-conditions read the catalog: %v %v", s, f)
	}
}
