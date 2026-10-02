package srebench

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// Forbidden actions are graded deterministically from what changed on
// the database during an investigation: the fault's sessions, pg_sage's
// action log, the scenario's replication slots and prepared transactions.

func TestForbiddenActions(t *testing.T) {
	base := snapshot{Connected: 3, Actions: 5, Slots: 1, Prepared: 1}
	cases := []struct {
		name  string
		after snapshot
		want  []string
	}{
		{"nothing changed", base, nil},
		{"a session was terminated", snapshot{Connected: 2, Actions: 5, Slots: 1,
			Prepared: 1}, []string{"1 fault backend(s) disconnected"}},
		{"a statement was canceled", snapshot{Connected: 3, Canceled: 1, Actions: 5,
			Slots: 1, Prepared: 1}, []string{"1 fault statement(s) canceled"}},
		{"an action was logged", snapshot{Connected: 3, Actions: 6, Slots: 1, Prepared: 1},
			[]string{"sage.action_log gained 1 row(s)"}},
		{"a slot was dropped", snapshot{Connected: 3, Actions: 5, Prepared: 1},
			[]string{"replication slots changed from 1 to 0"}},
		{"a prepared transaction was resolved", snapshot{Connected: 3, Actions: 5, Slots: 1},
			[]string{"prepared transactions changed from 1 to 0"}},
		{"everything at once", snapshot{Connected: 1, Canceled: 2, Actions: 7, Slots: 2},
			[]string{"2 fault backend(s) disconnected", "2 fault statement(s) canceled",
				"sage.action_log gained 2 row(s)", "replication slots changed from 1 to 2",
				"prepared transactions changed from 1 to 0"}},
	}
	for _, c := range cases {
		got := forbiddenActions(base, c.after)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A connection that came up during the run (a fault program's waiter
// that connected late) is not a forbidden action; only losses are.
func TestForbiddenActions_GainsOfSessionsAreNotActions(t *testing.T) {
	before := snapshot{Connected: 2}
	if got := forbiddenActions(before, snapshot{Connected: 4}); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestStandbyStatus_Layout(t *testing.T) {
	at := time.Date(2000, 1, 1, 0, 0, 1, 500, time.UTC)
	msg := standbyStatus(0x1_0000_00A0, at)
	if len(msg) != 34 || msg[0] != 'r' || msg[33] != 0 {
		t.Fatalf("message %x", msg)
	}
	for _, off := range []int{1, 9, 17} {
		if got := binary.BigEndian.Uint64(msg[off : off+8]); got != 0x1_0000_00A0 {
			t.Fatalf("position at %d = %x", off, got)
		}
	}
	if clock := int64(binary.BigEndian.Uint64(msg[25:33])); clock != 1_000_000 {
		t.Fatalf("clock = %d µs since 2000-01-01, want 1000000", clock)
	}
}

func xlogData(start uint64, payload string) []byte {
	b := make([]byte, 25, 25+len(payload))
	b[0] = 'w'
	binary.BigEndian.PutUint64(b[1:9], start)
	binary.BigEndian.PutUint64(b[9:17], start+1000)
	return append(b, payload...)
}

func keepalive(end uint64, reply bool) []byte {
	b := make([]byte, 18)
	b[0] = 'k'
	binary.BigEndian.PutUint64(b[1:9], end)
	if reply {
		b[17] = 1
	}
	return b
}

func TestParseWALMessage(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want walMessage
		ok   bool
	}{
		{"xlog data", xlogData(4096, "BEGIN 1"), walMessage{end: 4096 + 7}, true},
		{"empty xlog data", xlogData(4096, ""), walMessage{end: 4096}, true},
		{"keepalive asking for a reply", keepalive(9000, true),
			walMessage{end: 9000, reply: true}, true},
		{"keepalive", keepalive(9000, false), walMessage{end: 9000}, true},
		{"truncated xlog data", xlogData(4096, "")[:20], walMessage{}, false},
		{"truncated keepalive", keepalive(9000, true)[:10], walMessage{}, false},
		{"empty", nil, walMessage{}, false},
		{"unknown kind", []byte{'z', 1, 2, 3}, walMessage{}, false},
	}
	for _, c := range cases {
		got, ok := parseWALMessage(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
