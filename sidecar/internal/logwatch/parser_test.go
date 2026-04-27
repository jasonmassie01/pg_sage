package logwatch

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ParseJSONLogLine
// ---------------------------------------------------------------------------

func TestParseJSONLogLine_Valid(t *testing.T) {
	line := []byte(`{
		"timestamp":"2024-03-10T14:30:00.123+00:00",
		"pid":1234,
		"session_id":"abc123",
		"database_name":"mydb",
		"user_name":"postgres",
		"error_severity":"ERROR",
		"state_code":"40P01",
		"message":"deadlock detected",
		"detail":"Process 1234 waits for ...",
		"hint":"See server log",
		"query":"SELECT 1",
		"application_name":"myapp"
	}`)
	entry, err := ParseJSONLogLine(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.PID != 1234 {
		t.Errorf("PID = %d, want 1234", entry.PID)
	}
	if entry.SessionID != "abc123" {
		t.Errorf("SessionID = %q, want %q", entry.SessionID, "abc123")
	}
	if entry.Database != "mydb" {
		t.Errorf("Database = %q, want %q", entry.Database, "mydb")
	}
	if entry.User != "postgres" {
		t.Errorf("User = %q, want %q", entry.User, "postgres")
	}
	if entry.ErrorLevel != "ERROR" {
		t.Errorf("ErrorLevel = %q, want %q", entry.ErrorLevel, "ERROR")
	}
	if entry.SQLState != "40P01" {
		t.Errorf("SQLState = %q, want %q", entry.SQLState, "40P01")
	}
	if entry.Message != "deadlock detected" {
		t.Errorf("Message = %q, want %q", entry.Message, "deadlock detected")
	}
	if entry.Detail != "Process 1234 waits for ..." {
		t.Errorf("Detail = %q, want %q", entry.Detail, "Process 1234 waits for ...")
	}
	if entry.Hint != "See server log" {
		t.Errorf("Hint = %q, want %q", entry.Hint, "See server log")
	}
	if entry.Query != "SELECT 1" {
		t.Errorf("Query = %q, want %q", entry.Query, "SELECT 1")
	}
	if entry.Application != "myapp" {
		t.Errorf("Application = %q, want %q", entry.Application, "myapp")
	}
	// Timestamp should be in UTC.
	if entry.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v, want UTC", entry.Timestamp.Location())
	}
	wantTS := time.Date(2024, 3, 10, 14, 30, 0, 123000000, time.UTC)
	if !entry.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", entry.Timestamp, wantTS)
	}
}

func TestParseJSONLogLine_MalformedJSON(t *testing.T) {
	line := []byte(`{not json}`)
	_, err := ParseJSONLogLine(line)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "json unmarshal") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "json unmarshal")
	}
}

func TestParseJSONLogLine_MissingTimestamp(t *testing.T) {
	line := []byte(`{"pid":1,"error_severity":"ERROR","message":"boom"}`)
	_, err := ParseJSONLogLine(line)
	if err == nil {
		t.Fatal("expected error for missing timestamp, got nil")
	}
	if !strings.Contains(err.Error(), "parse timestamp") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parse timestamp")
	}
}

func TestParseJSONLogLine_NonUTF8(t *testing.T) {
	// Embed invalid byte sequence inside an otherwise valid JSON line.
	raw := []byte(`{"timestamp":"2024-03-10T14:30:00.000+00:00","pid":1,` +
		`"error_severity":"LOG","message":"bad ` + "\x80\xfe" + ` byte"}`)
	entry, err := ParseJSONLogLine(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Non-UTF-8 bytes should be replaced with U+FFFD.
	if !strings.Contains(entry.Message, "\uFFFD") {
		t.Errorf("expected replacement char in message, got %q", entry.Message)
	}
}

func TestParseJSONLogLine_EmptyInput(t *testing.T) {
	_, err := ParseJSONLogLine([]byte(""))
	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

func TestParseJSONLogLine_AllFieldsPopulated(t *testing.T) {
	line := []byte(`{
		"timestamp":"2024-01-01T00:00:00.000+00:00",
		"pid":99,
		"session_id":"sess1",
		"database_name":"db1",
		"user_name":"user1",
		"error_severity":"FATAL",
		"state_code":"53300",
		"message":"msg1",
		"detail":"det1",
		"hint":"hint1",
		"query":"q1",
		"application_name":"app1"
	}`)
	entry, err := ParseJSONLogLine(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		field string
		got   string
		want  string
	}{
		{"SessionID", entry.SessionID, "sess1"},
		{"Database", entry.Database, "db1"},
		{"User", entry.User, "user1"},
		{"ErrorLevel", entry.ErrorLevel, "FATAL"},
		{"SQLState", entry.SQLState, "53300"},
		{"Message", entry.Message, "msg1"},
		{"Detail", entry.Detail, "det1"},
		{"Hint", entry.Hint, "hint1"},
		{"Query", entry.Query, "q1"},
		{"Application", entry.Application, "app1"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if entry.PID != 99 {
		t.Errorf("PID = %d, want 99", entry.PID)
	}
}

// ---------------------------------------------------------------------------
// ParseCSVLogLine
// ---------------------------------------------------------------------------

// makeCSVRecord builds a 23-column CSV record with sensible defaults.
func makeCSVRecord(overrides map[int]string) []string {
	rec := make([]string, 23)
	// Defaults: time, user, database, pid, ..., severity, sqlstate, message, ...
	rec[csvColTime] = "2024-03-10 14:30:00.123 UTC"
	rec[csvColUser] = "postgres"
	rec[csvColDatabase] = "mydb"
	rec[csvColPID] = "5678"
	rec[csvColSessionID] = "sess456"
	rec[csvColSeverity] = "ERROR"
	rec[csvColSQLState] = "40P01"
	rec[csvColMessage] = "deadlock detected"
	rec[csvColDetail] = "detail text"
	rec[csvColHint] = "hint text"
	rec[csvColQuery] = "SELECT 1"
	rec[csvColApplication] = "testapp"
	for k, v := range overrides {
		rec[k] = v
	}
	return rec
}

func TestParseCSVLogLine_Valid23Columns(t *testing.T) {
	rec := makeCSVRecord(nil)
	entry, err := ParseCSVLogLine(rec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.PID != 5678 {
		t.Errorf("PID = %d, want 5678", entry.PID)
	}
	if entry.Database != "mydb" {
		t.Errorf("Database = %q, want %q", entry.Database, "mydb")
	}
	if entry.ErrorLevel != "ERROR" {
		t.Errorf("ErrorLevel = %q, want %q", entry.ErrorLevel, "ERROR")
	}
	if entry.SQLState != "40P01" {
		t.Errorf("SQLState = %q, want %q", entry.SQLState, "40P01")
	}
	if entry.Message != "deadlock detected" {
		t.Errorf("Message = %q, want %q", entry.Message, "deadlock detected")
	}
	wantTS := time.Date(2024, 3, 10, 14, 30, 0, 123000000, time.UTC)
	if !entry.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", entry.Timestamp, wantTS)
	}
}

func TestParseCSVLogLine_Valid26Columns(t *testing.T) {
	// PG14+ has 26 columns.
	rec := make([]string, 26)
	copy(rec, makeCSVRecord(nil))
	rec[23] = "client_backend"
	rec[24] = "0"
	rec[25] = "12345"
	entry, err := ParseCSVLogLine(rec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry.PID != 5678 {
		t.Errorf("PID = %d, want 5678", entry.PID)
	}
	if entry.Database != "mydb" {
		t.Errorf("Database = %q, want %q", entry.Database, "mydb")
	}
}

func TestParseCSVLogLine_TooFewColumns(t *testing.T) {
	rec := make([]string, 5) // far too few
	_, err := ParseCSVLogLine(rec)
	if err == nil {
		t.Fatal("expected error for too few columns, got nil")
	}
	if !strings.Contains(err.Error(), "columns") {
		t.Errorf("error = %q, want it to mention columns", err.Error())
	}
}

func TestParseCSVLogLine_InvalidPID(t *testing.T) {
	rec := makeCSVRecord(map[int]string{csvColPID: "notanumber"})
	_, err := ParseCSVLogLine(rec)
	if err == nil {
		t.Fatal("expected error for invalid PID, got nil")
	}
	if !strings.Contains(err.Error(), "parse pid") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parse pid")
	}
}

func TestParseCSVLogLine_InvalidTimestamp(t *testing.T) {
	rec := makeCSVRecord(map[int]string{csvColTime: "not-a-timestamp"})
	_, err := ParseCSVLogLine(rec)
	if err == nil {
		t.Fatal("expected error for invalid timestamp, got nil")
	}
	if !strings.Contains(err.Error(), "parse timestamp") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parse timestamp")
	}
}

func TestParseCSVLogLine_TimestampFormats(t *testing.T) {
	formats := []struct {
		ts   string
		desc string
	}{
		{"2024-03-10 14:30:00.123 UTC", "MST format"},
		{"2024-03-10 14:30:00.123-00", "minus offset"},
		{"2024-03-10 14:30:00.123+00", "plus offset"},
		{"2024-03-10 14:30:00.123-00:00", "minus offset with colon"},
		{"2024-03-10 14:30:00.123+00:00", "plus offset with colon"},
	}
	for _, f := range formats {
		t.Run(f.desc, func(t *testing.T) {
			rec := makeCSVRecord(map[int]string{csvColTime: f.ts})
			entry, err := ParseCSVLogLine(rec)
			if err != nil {
				t.Fatalf("ParseCSVLogLine(%q): %v", f.ts, err)
			}
			if entry.Timestamp.IsZero() {
				t.Fatalf("timestamp is zero for format %q", f.ts)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ShouldParseLine
// ---------------------------------------------------------------------------

func TestShouldParseLine_HighSeverity(t *testing.T) {
	for _, sev := range []string{"ERROR", "FATAL", "PANIC", "WARNING"} {
		if !ShouldParseLine(sev, "anything") {
			t.Errorf("ShouldParseLine(%q, ...) = false, want true", sev)
		}
	}
}

func TestShouldParseLine_HighSeverity_LowerCase(t *testing.T) {
	// Verify case-insensitivity for severity.
	for _, sev := range []string{"error", "fatal", "panic", "warning"} {
		if !ShouldParseLine(sev, "anything") {
			t.Errorf("ShouldParseLine(%q, ...) = false, want true", sev)
		}
	}
}

func TestShouldParseLine_LOG_WithKeyword(t *testing.T) {
	if !ShouldParseLine("LOG", "temporary file: size 12345") {
		t.Fatal("LOG with keyword should return true")
	}
}

func TestShouldParseLine_LOG_WithoutKeyword(t *testing.T) {
	if ShouldParseLine("LOG", "connection received: host=1.2.3.4") {
		t.Fatal("LOG without keyword should return false")
	}
}

func TestShouldParseLine_DEBUG_False(t *testing.T) {
	if ShouldParseLine("DEBUG", "deadlock detected") {
		t.Fatal("DEBUG should return false regardless of message")
	}
}

func TestShouldParseLine_NOTICE_False(t *testing.T) {
	if ShouldParseLine("NOTICE", "deadlock detected") {
		t.Fatal("NOTICE should return false regardless of message")
	}
}

func TestShouldParseLine_EmptySeverity(t *testing.T) {
	if ShouldParseLine("", "deadlock detected") {
		t.Fatal("empty severity should return false")
	}
}

// ---------------------------------------------------------------------------
// containsLogKeyword
// ---------------------------------------------------------------------------

func TestContainsLogKeyword_EachKeyword(t *testing.T) {
	keywords := []string{
		"deadlock",
		"temporary file",
		"checkpoint",
		"archive",
		"autovacuum",
		"replication slot",
		"vacuum",
		"terminated by signal",
		"duration:",
	}
	for _, kw := range keywords {
		if !containsLogKeyword(kw) {
			t.Errorf("containsLogKeyword(%q) = false, want true", kw)
		}
	}
}

func TestContainsLogKeyword_CaseInsensitive(t *testing.T) {
	if !containsLogKeyword("DEADLOCK DETECTED") {
		t.Fatal("containsLogKeyword should be case-insensitive")
	}
	if !containsLogKeyword("Temporary File: path /tmp/...") {
		t.Fatal("containsLogKeyword should be case-insensitive")
	}
}

func TestContainsLogKeyword_NoMatch(t *testing.T) {
	if containsLogKeyword("connection received from 10.0.0.1") {
		t.Fatal("expected no match for irrelevant message")
	}
}

func TestContainsLogKeyword_EmptyMessage(t *testing.T) {
	if containsLogKeyword("") {
		t.Fatal("expected false for empty message")
	}
}
