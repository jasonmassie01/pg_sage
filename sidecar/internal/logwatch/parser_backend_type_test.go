package logwatch

import "testing"

func TestParseCSVLogLine_BackendTypeFromColumn23(t *testing.T) {
	rec := make([]string, 26)
	copy(rec, makeCSVRecord(nil))
	rec[23] = "postmaster"
	e, err := ParseCSVLogLine(rec)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.BackendType != "postmaster" {
		t.Fatalf("BackendType = %q, want postmaster", e.BackendType)
	}
}

// A 23-column (pre-PG13) record has no backend_type column.
func TestParseCSVLogLine_NoBackendTypeColumn(t *testing.T) {
	e, err := ParseCSVLogLine(makeCSVRecord(nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.BackendType != "" {
		t.Fatalf("BackendType = %q, want empty", e.BackendType)
	}
}

func TestParseJSONLogLine_BackendType(t *testing.T) {
	line := `{"timestamp":"2026-11-08 20:07:05.449 UTC","pid":1,` +
		`"error_severity":"LOG","message":"server process (PID 108) was ` +
		`terminated by signal 9: Killed","backend_type":"postmaster"}`
	e, err := ParseJSONLogLine([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.BackendType != "postmaster" || e.User != "" || e.Database != "" {
		t.Fatalf("entry = %+v, want postmaster with no user/database", e)
	}
}
