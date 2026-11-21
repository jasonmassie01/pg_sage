package notify

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// ux-09b: the weekly line "N messages, M led to an action". N counts the
// sent deliveries of the last 7 days; M those followed within 24 h by a
// decision on the same item (or a mute of the same database). No
// concurrent access test: ComputeWeeklyLine is a pure function of its
// arguments; the database read is covered in weekly_line_db_test.go.

var lineNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func sentAt(ago time.Duration, db string, items ...string) LineDelivery {
	return LineDelivery{SentAt: lineNow.Add(-ago), Database: db, Items: items}
}

func TestWeeklyLine_NoMessagesSaysSo(t *testing.T) {
	for _, ds := range [][]LineDelivery{nil, {}, {sentAt(8*24*time.Hour, "orders")}} {
		got := ComputeWeeklyLine(ds, nil, lineNow)
		if got.Messages != 0 || got.LedToAction != 0 || got.Text != "No messages this week" {
			t.Fatalf("ComputeWeeklyLine(%v) = %+v, want No messages this week", ds, got)
		}
	}
}

func TestWeeklyLine_CountsMessagesAndActions(t *testing.T) {
	ds := []LineDelivery{
		sentAt(48*time.Hour, "orders", "queue:1"),
		sentAt(47*time.Hour, "orders", "queue:2", "queue:3"),
		sentAt(10*time.Hour, "billing"),
	}
	decs := []LineDecision{
		{At: lineNow.Add(-46 * time.Hour), Item: "queue:3", Verb: "approve"},
		{At: lineNow.Add(-40 * time.Hour), Item: "queue:9", Verb: "reject"},
	}
	got := ComputeWeeklyLine(ds, decs, lineNow)
	if got.Messages != 3 || got.LedToAction != 1 || got.Text != "3 messages, 1 led to an action" {
		t.Fatalf("got %+v, want 3 messages, 1 led to an action", got)
	}
	if !got.Since.Equal(lineNow.Add(-7*24*time.Hour)) || !got.Until.Equal(lineNow) {
		t.Fatalf("window %v..%v, want the last 7 days", got.Since, got.Until)
	}
	one := ComputeWeeklyLine(ds[2:], nil, lineNow)
	if one.Text != "1 message, 0 led to an action" {
		t.Fatalf("one message renders %q", one.Text)
	}
}

// Boundary: exactly 24 h after a message still counts; 24 h + 1 s and a
// decision before the message do not.
func TestWeeklyLine_TwentyFourHourBoundary(t *testing.T) {
	d := sentAt(72*time.Hour, "orders", "queue:5")
	cases := []struct {
		after time.Duration
		want  int
	}{
		{0, 1},
		{time.Second, 1},
		{24 * time.Hour, 1},
		{24*time.Hour + time.Second, 0},
		{-time.Second, 0},
	}
	for _, c := range cases {
		dec := LineDecision{At: d.SentAt.Add(c.after), Item: "queue:5", Verb: "snooze"}
		if got := ComputeWeeklyLine([]LineDelivery{d}, []LineDecision{dec}, lineNow); got.
			LedToAction != c.want {
			t.Errorf("decision %v after: led %d, want %d", c.after, got.LedToAction, c.want)
		}
	}
}

// Boundary: the 7-day window starts exactly 7 days ago (inclusive).
func TestWeeklyLine_SevenDayWindow(t *testing.T) {
	ds := []LineDelivery{
		sentAt(7*24*time.Hour, "orders"),
		sentAt(7*24*time.Hour+time.Second, "orders"),
		sentAt(0, "orders"),
		{SentAt: lineNow.Add(time.Second), Database: "orders"},
	}
	if got := ComputeWeeklyLine(ds, nil, lineNow); got.Messages != 2 {
		t.Fatalf("counted %d messages, want the 7-days-ago and the current one", got.Messages)
	}
}

// A mute of the message's database counts as its action; an item decision
// does not match by database alone.
func TestWeeklyLine_MuteMatchesTheDatabase(t *testing.T) {
	ds := []LineDelivery{sentAt(30*time.Hour, "orders"), sentAt(30*time.Hour, "billing"),
		sentAt(30*time.Hour, "")}
	decs := []LineDecision{
		{At: lineNow.Add(-29 * time.Hour), Database: "orders", Verb: "mute"},
		{At: lineNow.Add(-29 * time.Hour), Database: "billing", Verb: "approve"},
		{At: lineNow.Add(-29 * time.Hour), Database: "", Verb: "mute"},
	}
	if got := ComputeWeeklyLine(ds, decs, lineNow); got.Messages != 3 || got.LedToAction != 1 {
		t.Fatalf("got %+v, want only the orders message led to an action", got)
	}
}

func TestEventItemRefs(t *testing.T) {
	cases := []struct {
		evt  Event
		want []string
	}{
		{Event{Data: map[string]any{"queue_id": 7}}, []string{"queue:7"}},
		{Event{Data: map[string]any{"queue_id": int64(8)}}, []string{"queue:8"}},
		{Event{Data: map[string]any{"queue_id": float64(9)}}, []string{"queue:9"}},
		{Event{Data: map[string]any{"queue_id": 0}}, nil},
		{Event{Data: map[string]any{"queue_id": "x"}}, nil},
		{Event{Data: map[string]any{DataItemRefs: []string{"queue:1", "queue:2"}}},
			[]string{"queue:1", "queue:2"}},
		{Event{Data: map[string]any{DataItemRefs: []any{"queue:3", 4, ""}}},
			[]string{"queue:3"}},
		{Event{Data: map[string]any{"queue_id": 1, DataItemRefs: []string{"queue:1"}}},
			[]string{"queue:1"}},
		{Event{}, nil},
	}
	for _, c := range cases {
		if got := eventItemRefs(c.evt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("eventItemRefs(%v) = %v, want %v", c.evt.Data, got, c.want)
		}
	}
}

// A batched Ask carries the item of every Ask it lists.
func TestAskBatchMessageCarriesItsItems(t *testing.T) {
	b := askBatch{Count: 2, Items: []storedEvent{
		{Subject: "a", Data: map[string]any{"queue_id": 11}},
		{Subject: "b", Data: map[string]any{"queue_id": float64(12)}},
	}}
	got := eventItemRefs(b.message("orders", "ask-batch:orders"))
	if !reflect.DeepEqual(got, []string{"queue:11", "queue:12"}) {
		t.Fatalf("batch items %v, want queue:11 and queue:12", got)
	}
}

func TestMuteLink(t *testing.T) {
	if got := MuteLink("orders db", ClassAsk); got != "#/notifications?class=ask&mute=orders+db" {
		t.Fatalf("MuteLink = %q", got)
	}
	if got := MuteLink("orders", ""); got != "#/notifications?mute=orders" {
		t.Fatalf("MuteLink every class = %q", got)
	}
	b := askBatch{Count: 2, Items: []storedEvent{{Subject: "a"}, {Subject: "b"}}}
	if body := b.message("orders", "k").Body; !strings.Contains(body,
		"Mute this class: #/notifications?class=ask&mute=orders") {
		t.Fatalf("batch footer %q lacks the mute link", body)
	}
}

// Review (ux-09b): one item decided several times, listed out of order,
// counts when any decision falls in its 24 h; decisions on either side of
// the window do not. A mute of another database never counts.
func TestWeeklyLine_UnsortedDecisionsOnOneItem(t *testing.T) {
	d := sentAt(72*time.Hour, "orders", "queue:5")
	at := func(off time.Duration, verb string) LineDecision {
		return LineDecision{At: d.SentAt.Add(off), Item: "queue:5", Verb: verb}
	}
	outside := []LineDecision{at(30*time.Hour, "reject"), at(-time.Hour, "approve"),
		{At: d.SentAt.Add(time.Hour), Database: "billing", Verb: "mute"},
		{At: d.SentAt.Add(time.Hour), Item: "queue:6", Verb: "snooze"},
		{At: d.SentAt.Add(time.Hour), Verb: "mute"}}
	if got := ComputeWeeklyLine([]LineDelivery{d}, outside, lineNow); got.LedToAction != 0 {
		t.Fatalf("decisions outside the window led %d, want 0", got.LedToAction)
	}
	inside := append([]LineDecision{at(30*time.Hour, "snooze")}, outside...)
	inside = append(inside, at(23*time.Hour, "approve"), at(-2*time.Hour, "snooze"))
	got := ComputeWeeklyLine([]LineDelivery{d, d}, inside, lineNow)
	if got.Messages != 2 || got.LedToAction != 2 {
		t.Fatalf("got %+v, want 2 messages, both led to an action", got)
	}
}
