package providerobs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// clouds-10: the log query names the database, malformed rows are skipped
// and counted instead of failing the batch.

const dbPredicate = "log_attributes['parsed.database_name'] = "

// sqlFixtureClient answers each logs request from the request's sql
// parameter and records every query sent.
func sqlFixtureClient(t *testing.T, answer func(sql string) string) (*Supabase, *[]string) {
	t.Helper()
	c, err := NewSupabase("project123", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sql := r.URL.Query().Get("sql")
		mu.Lock()
		sent = append(sent, sql)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(answer(sql)))}, nil
	})
	return c, &sent
}

func logRows(n int, database string, at time.Time) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"timestamp":%q,"database":%q,"event_message":"m%d"}`,
			at.Format(time.RFC3339Nano), database, i)
	}
	return `{"result":[` + strings.Join(rows, ",") + `]}`
}

func TestSupabaseLogsSkipMalformedTimestamp(t *testing.T) {
	body := `{"result":[
		{"timestamp":"2026-09-07T12:00:01Z","database":"postgres","event_message":"a"},
		{"timestamp":"wrong","database":"postgres","event_message":"b"},
		{"timestamp":"2026-09-07T12:00:03Z","database":"postgres","event_message":"c"}]}`
	c := fixtureClient(t, 200, body)
	from := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	entries, rejected, err := c.Logs(context.Background(), "postgres", from, from.Add(time.Minute))
	if err != nil {
		t.Fatalf("one malformed row failed the batch: %v", err)
	}
	if rejected != 1 || len(entries) != 2 || entries[0].Message != "a" ||
		entries[1].Message != "c" {
		t.Fatalf("entries = %#v rejected = %d, want a and c with 1 rejected", entries, rejected)
	}
}

func TestSupabaseLogsSkipMalformedProcessID(t *testing.T) {
	body := `{"result":[
		{"timestamp":"2026-09-07T12:00:01Z","database":"postgres","pid":"x"},
		{"timestamp":"2026-09-07T12:00:02Z","database":"postgres","pid":"-4"},
		{"timestamp":"2026-09-07T12:00:03Z","database":"postgres","pid":"7"},
		{"timestamp":"wrong","database":"other","pid":"x"}]}`
	c := fixtureClient(t, 200, body)
	from := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	entries, rejected, err := c.Logs(context.Background(), "postgres", from, from.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// The other database's malformed row is not ours to count.
	if rejected != 2 || len(entries) != 1 || entries[0].PID != 7 {
		t.Fatalf("entries = %#v rejected = %d, want pid 7 with 2 rejected", entries, rejected)
	}
}

func TestSupabaseLogsAllRowsMalformed(t *testing.T) {
	c := fixtureClient(t, 200, `{"result":[{"database":"postgres","timestamp":"wrong"}]}`)
	now := time.Now().UTC()
	entries, rejected, err := c.Logs(context.Background(), "postgres", now.Add(-time.Minute), now)
	if err != nil || rejected != 1 || len(entries) != 0 {
		t.Fatalf("entries = %#v rejected = %d err = %v", entries, rejected, err)
	}
}

func TestSupabaseLogsQueryNamesDatabase(t *testing.T) {
	cases := []struct {
		database, want string
		filtered       bool
	}{
		{"postgres", dbPredicate + "'postgres'", true},
		{"a'b", dbPredicate + "'a''b'", true},
		{"''", dbPredicate + "''''''", true},
		{`a\b`, "", false},
		{"a\x00b", "", false},
		{"a\nb", "", false},
		{"a\tb", "", false},
		{"a\x7fb", "", false},
		{"a\xffb", "", false},
	}
	for _, tc := range cases {
		c, sent := sqlFixtureClient(t, func(string) string { return `{"result":[]}` })
		now := time.Now().UTC()
		if _, _, err := c.Logs(context.Background(), tc.database, now.Add(-time.Minute),
			now); err != nil {
			t.Fatalf("%q: %v", tc.database, err)
		}
		sql := (*sent)[0]
		if got := strings.Contains(sql, dbPredicate); got != tc.filtered {
			t.Errorf("%q: filtered = %v, want %v; sql = %s", tc.database, got, tc.filtered, sql)
		}
		if tc.filtered && !strings.Contains(sql, "source = 'postgres_logs' AND "+tc.want+
			" ORDER BY timestamp ASC LIMIT 1000") {
			t.Errorf("%q: predicate misplaced or unescaped: %s", tc.database, sql)
		}
		if !strings.Contains(sql, "FROM logs WHERE source = 'postgres_logs'") {
			t.Errorf("%q: source filter lost: %s", tc.database, sql)
		}
	}
}

// A busy neighbour no longer crowds the database out of the row cap.
func TestSupabaseLogsFilteredQueryAvoidsNeighbourSaturation(t *testing.T) {
	from := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	at := from.Add(time.Second)
	for _, neighbours := range []int{maxLogRecords - 1, maxLogRecords} {
		c, _ := sqlFixtureClient(t, func(sql string) string {
			if strings.Contains(sql, dbPredicate+"'postgres'") {
				return logRows(1, "postgres", at)
			}
			return logRows(neighbours, "busy_neighbour", at)
		})
		entries, rejected, err := c.Logs(context.Background(), "postgres", from,
			from.Add(time.Minute))
		if err != nil || rejected != 0 || len(entries) != 1 ||
			entries[0].Database != "postgres" {
			t.Fatalf("%d neighbours: entries = %#v rejected = %d err = %v",
				neighbours, entries, rejected, err)
		}
	}
}

// The cap still holds for the filtered query itself.
func TestSupabaseLogsFilteredQueryStillSaturates(t *testing.T) {
	from := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		rows      int
		saturated bool
	}{{maxLogRecords - 1, false}, {maxLogRecords, true}} {
		c, _ := sqlFixtureClient(t, func(string) string {
			return logRows(tc.rows, "postgres", from.Add(time.Second))
		})
		entries, _, err := c.Logs(context.Background(), "postgres", from, from.Add(time.Minute))
		if got := err == ErrLogWindowSaturated; got != tc.saturated {
			t.Fatalf("%d rows: err = %v, want saturated %v", tc.rows, err, tc.saturated)
		}
		if !tc.saturated && len(entries) != tc.rows {
			t.Fatalf("%d rows: %d entries", tc.rows, len(entries))
		}
	}
}

// The runtime advances past a batch with a malformed row and reports it.
func TestRuntimeAdvancesPastMalformedSupabaseRows(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 3, 30, 0, time.UTC)
	until := now.Truncate(time.Minute).Add(-time.Minute)
	good := until.Add(-30 * time.Second).Format(time.RFC3339Nano)
	c, _ := sqlFixtureClient(t, func(string) string {
		return `{"result":[{"timestamp":"` + good + `","database":"postgres","event_message":"a"},
			{"timestamp":"wrong","database":"postgres","event_message":"b"},
			{"timestamp":"` + good + `","database":"postgres","event_message":"c"}]}`
	})
	var delivered []logwatch.LogEntry
	r, err := NewRuntime(c, "postgres", func(_ context.Context, e []logwatch.LogEntry) error {
		delivered = append(delivered, e...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var reports []string
	r.SetRejectionReporter(func(rejected int, total uint64) {
		reports = append(reports, fmt.Sprintf("%d/%d", rejected, total))
	})
	if err := r.pollLogs(context.Background(), now); err != nil {
		t.Fatalf("malformed row failed the poll: %v", err)
	}
	if !r.through.Equal(until) || len(delivered) != 2 {
		t.Fatalf("through = %v (want %v), delivered %d", r.through, until, len(delivered))
	}
	if len(reports) != 1 || reports[0] != "1/1" || r.RejectedLogRecords() != 1 {
		t.Fatalf("reports = %v total = %d, want one aggregated report", reports,
			r.RejectedLogRecords())
	}
}
