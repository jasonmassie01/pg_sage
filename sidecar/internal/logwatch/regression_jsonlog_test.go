package logwatch

import (
	"strings"
	"testing"
	"time"
)

// realJSONLogLine is shaped exactly like PostgreSQL's jsonlog output
// (src/backend/utils/error/jsonlog.c): the timestamp uses the same
// "%Y-%m-%d %H:%M:%S.mmm %Z" layout as csvlog, and the keys are
// user/dbname/statement — not user_name/database_name/query.
const realJSONLogLine = `{"timestamp":"2024-03-10 14:30:00.123 UTC",` +
	`"user":"app","dbname":"prod","pid":1234,"remote_host":"10.0.0.1",` +
	`"remote_port":51234,"session_id":"65edc3a8.4d2","line_num":3,` +
	`"ps":"UPDATE","session_start":"2024-03-10 14:29:59 UTC","vxid":"3/7",` +
	`"txid":742,"error_severity":"ERROR","state_code":"40P01",` +
	`"message":"deadlock detected",` +
	`"detail":"Process 1234 waits for ShareLock on transaction 743.",` +
	`"hint":"See server log for query details.",` +
	`"context":"while updating tuple (0,1) in relation \"t\"",` +
	`"statement":"UPDATE t SET v = 1 WHERE id = 1",` +
	`"application_name":"myapp","backend_type":"client backend",` +
	`"query_id":-6317463522411371009}`

// G1-B02: every real PostgreSQL jsonlog line must parse with all fields.
func TestParseJSONLogLine_RealPostgresKeys(t *testing.T) {
	entry, err := ParseJSONLogLine([]byte(realJSONLogLine))
	if err != nil {
		t.Fatalf("real PostgreSQL jsonlog line rejected: %v", err)
	}
	want := time.Date(2024, 3, 10, 14, 30, 0, 123e6, time.UTC)
	if !entry.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", entry.Timestamp, want)
	}
	checks := map[string][2]string{
		"Database":    {entry.Database, "prod"},
		"User":        {entry.User, "app"},
		"Query":       {entry.Query, "UPDATE t SET v = 1 WHERE id = 1"},
		"ErrorLevel":  {entry.ErrorLevel, "ERROR"},
		"SQLState":    {entry.SQLState, "40P01"},
		"Message":     {entry.Message, "deadlock detected"},
		"Detail":      {entry.Detail, "Process 1234 waits for ShareLock on transaction 743."},
		"Hint":        {entry.Hint, "See server log for query details."},
		"SessionID":   {entry.SessionID, "65edc3a8.4d2"},
		"Application": {entry.Application, "myapp"},
	}
	for field, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", field, pair[0], pair[1])
		}
	}
	if entry.PID != 1234 {
		t.Errorf("PID = %d, want 1234", entry.PID)
	}
}

// G1-B02: the fictional ISO-8601 timestamp is not what PostgreSQL writes;
// a missing timestamp must be an error, never a zero time.
func TestParseJSONLogLine_MissingTimestampRejected(t *testing.T) {
	_, err := ParseJSONLogLine([]byte(`{"pid":1,"error_severity":"ERROR"}`))
	if err == nil {
		t.Fatal("expected error for jsonlog line without timestamp")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Errorf("error %q should mention the timestamp", err)
	}
}

// G1-B24: zone abbreviations must resolve to their real UTC offset instead
// of silently parsing as UTC (Go's behavior for abbreviations it does not
// know in the local zone).
func TestParsePostgresTimestamp_ZoneAbbreviations(t *testing.T) {
	want := time.Date(2024, 3, 10, 14, 30, 0, 123e6, time.UTC)
	cases := []string{
		"2024-03-10 14:30:00.123 UTC",
		"2024-03-10 14:30:00.123 GMT",
		"2024-03-10 09:30:00.123 EST",
		"2024-03-10 10:30:00.123 EDT",
		"2024-03-10 07:30:00.123 PDT",
		"2024-03-10 15:30:00.123 CET",
		"2024-03-10 16:30:00.123 CEST",
		"2024-03-10 23:30:00.123 JST",
		"2024-03-11 01:30:00.123 AEDT",
		"2024-03-10 20:00:00.123 +0530",
		"2024-03-10 11:30:00.123 -03",
		"2024-03-10 20:00:00.123 +05:30",
	}
	for _, in := range cases {
		got, err := parseCSVTimestamp(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%q parsed as %v, want %v", in, got, want)
		}
	}
}

// G1-B24: an abbreviation with no known offset must be reported, not
// mis-stamped as UTC.
func TestParsePostgresTimestamp_UnknownAbbreviationRejected(t *testing.T) {
	got, err := parseCSVTimestamp("2024-03-10 14:30:00.123 XYZT")
	if err == nil {
		t.Fatalf("unknown abbreviation accepted as %v; want error", got)
	}
	if !strings.Contains(err.Error(), "XYZT") {
		t.Errorf("error %q should name the unknown abbreviation", err)
	}
}

// G1-B14: the documented default for exclude_applications is pg_sage, so a
// classifier built without an explicit list must treat the sidecar's own
// statement timeouts as self-inflicted noise.
func TestClassifier_DefaultExcludesSidecarApplication(t *testing.T) {
	c := NewClassifier(ClassifierConfig{}, nil)
	sig := c.Classify(LogEntry{
		Timestamp:   time.Now().UTC(),
		PID:         42,
		Database:    "prod",
		ErrorLevel:  "ERROR",
		SQLState:    "57014",
		Message:     "canceling statement due to statement timeout",
		Application: "pg_sage",
	})
	if sig != nil {
		t.Fatalf("pg_sage's own timeout produced signal %q", sig.ID)
	}
	other := c.Classify(LogEntry{
		Timestamp:   time.Now().UTC(),
		PID:         43,
		Database:    "prod",
		ErrorLevel:  "ERROR",
		SQLState:    "57014",
		Message:     "canceling statement due to statement timeout",
		Application: "billing",
	})
	if other == nil || other.ID != "log_statement_timeout" {
		t.Fatalf("application timeout signal = %#v, want log_statement_timeout", other)
	}
}

// G1-B14: an explicit operator list replaces the default.
func TestClassifier_ExplicitExcludeListIsHonored(t *testing.T) {
	c := NewClassifier(ClassifierConfig{ExcludeApps: []string{"batch"}}, nil)
	sig := c.Classify(LogEntry{
		Timestamp: time.Now().UTC(), PID: 7, Database: "prod",
		ErrorLevel: "ERROR", SQLState: "57014",
		Message:     "canceling statement due to statement timeout",
		Application: "batch",
	})
	if sig != nil {
		t.Fatalf("excluded application produced signal %q", sig.ID)
	}
}
