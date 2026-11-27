package collector

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// collection-01: the locks category is a bounded wait graph whose
// statement text is redacted and cut before it is stored.

// No concurrent access tests: capLocks and redactLockQuery are pure
// functions of their arguments, and one collector goroutine owns the read.

func lockRows(n int) []LockInfo {
	rows := make([]LockInfo, n)
	for i := range rows {
		rows[i] = LockInfo{LockType: "virtualxid", Mode: "ExclusiveLock",
			Granted: true, PID: 1000 + i}
	}
	return rows
}

// Boundary: the read asks for one row past the cap to detect truncation.
func TestCapLocks_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		candidates, wantKept, wantDropped int
	}{
		{0, 0, 0},
		{1, 1, 0},
		{199, 199, 0},
		{200, 200, 0},
		{201, 200, 1},
	} {
		kept, dropped := capLocks(lockRows(tc.candidates))
		if len(kept) != tc.wantKept || dropped != tc.wantDropped {
			t.Errorf("%d candidates: kept %d dropped %d, want %d and %d",
				tc.candidates, len(kept), dropped, tc.wantKept, tc.wantDropped)
		}
		if tc.candidates > 0 && kept[len(kept)-1].PID != 1000+tc.wantKept-1 {
			t.Errorf("%d candidates: last kept pid %d, want the first %d rows in order",
				tc.candidates, kept[len(kept)-1].PID, tc.wantKept)
		}
	}
}

// Nil/empty: an empty read is stored as [] (no locks), never null.
func TestCapLocks_NilIsEmptyNotNull(t *testing.T) {
	kept, dropped := capLocks(nil)
	if kept == nil || len(kept) != 0 || dropped != 0 {
		t.Fatalf("capLocks(nil) = %#v, %d; want an empty, non-nil slice", kept, dropped)
	}
	doc, err := json.Marshal(kept)
	if err != nil || string(doc) != "[]" {
		t.Fatalf("empty locks document = %s (%v), want []", doc, err)
	}
}

func TestRedactLockQuery_RemovesLiterals(t *testing.T) {
	q := "SELECT * FROM accounts WHERE id = 'secret-123' AND note = $$hush$$"
	got := redactLockQuery(&q)
	if got == nil {
		t.Fatal("redactLockQuery returned nil for a statement")
	}
	if strings.Contains(*got, "secret-123") || strings.Contains(*got, "hush") {
		t.Fatalf("redacted text %q still holds a literal", *got)
	}
	want := "SELECT * FROM accounts WHERE id = '?' AND note = $?$"
	if *got != want {
		t.Fatalf("redacted text = %q, want %q", *got, want)
	}
	if q != "SELECT * FROM accounts WHERE id = 'secret-123' AND note = $$hush$$" {
		t.Fatal("redactLockQuery modified its argument")
	}
}

// Invalid input: a NULL query (no session text) stays NULL.
func TestRedactLockQuery_NilStaysNil(t *testing.T) {
	if got := redactLockQuery(nil); got != nil {
		t.Fatalf("redactLockQuery(nil) = %q, want nil", *got)
	}
	empty := ""
	if got := redactLockQuery(&empty); got == nil || *got != "" {
		t.Fatalf("redactLockQuery(\"\") = %v, want an empty string", got)
	}
}

// The text is redacted before it is cut: a cut through a literal must not
// leave its head behind.
func TestRedactLockQuery_RedactsBeforeCutting(t *testing.T) {
	q := "UPDATE t SET v = 1 WHERE " + strings.Repeat("x", 230) +
		" = 'secret-123-and-a-long-tail'"
	got := redactLockQuery(&q)
	if got == nil || strings.Contains(*got, "secret") {
		t.Fatalf("cut text %v holds the head of a literal", got)
	}
	if len(*got) > lockQueryMaxBytes {
		t.Fatalf("stored text is %d bytes, want at most %d", len(*got), lockQueryMaxBytes)
	}
}

// An unterminated literal (track_activity_query_size cut the statement) is
// redacted to its end.
func TestRedactLockQuery_UnterminatedLiteral(t *testing.T) {
	q := "INSERT INTO t VALUES ('secret-123 cut by the server"
	got := redactLockQuery(&q)
	if got == nil || strings.Contains(*got, "secret") {
		t.Fatalf("unterminated literal leaked: %v", got)
	}
}

// Boundary: 256 bytes stay whole, 257 are cut, and a cut never splits a
// UTF-8 sequence.
func TestRedactLockQuery_CutBoundaries(t *testing.T) {
	for _, n := range []int{255, 256, 257, 4096} {
		q := strings.Repeat("a", n)
		got := redactLockQuery(&q)
		want := min(n, lockQueryMaxBytes)
		if got == nil || len(*got) != want {
			t.Errorf("%d-byte text stored as %d bytes, want %d", n, len(deref(got)), want)
		}
	}
	q := strings.Repeat("a", 255) + "é" + "tail" // é is two bytes at 255..256
	got := redactLockQuery(&q)
	if got == nil || !utf8.ValidString(*got) || len(*got) != 255 {
		t.Fatalf("multi-byte cut = %q (%d bytes), want the 255 bytes before é",
			deref(got), len(deref(got)))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// The new fields are stored under stable names; a session row has no
// blocked_by list.
func TestLockInfo_WaitGraphFieldsMarshal(t *testing.T) {
	age, qid := int64(42), int64(-7)
	waiter := LockInfo{LockType: "transactionid", Mode: "ShareLock", PID: 2,
		BlockedBy: []int{1}, BackendXminAge: &age, QueryID: &qid}
	doc, err := json.Marshal(waiter)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"blocked_by":[1]`, `"backend_xmin_age":42`,
		`"query_id":-7`, `"xact_start":null`} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("waiter document %s lacks %s", doc, want)
		}
	}
	var back LockInfo
	if err := json.Unmarshal(doc, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.BlockedBy) != 1 || back.BlockedBy[0] != 1 || back.QueryID == nil ||
		*back.QueryID != -7 || back.BackendXminAge == nil || *back.BackendXminAge != 42 {
		t.Fatalf("round trip = %+v, want the wait-graph fields back", back)
	}
	session, err := json.Marshal(LockInfo{LockType: "virtualxid", Granted: true, PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(session), `"blocked_by":null`) {
		t.Errorf("session document %s, want blocked_by null", session)
	}
}

// The statement stays bounded and keeps pg_sage's self-exclusion.
func TestLocksSQL_BoundedWaitGraph(t *testing.T) {
	for _, want := range []string{
		"pg_blocking_pids", "LIMIT 200", "LIMIT 201", "AS MATERIALIZED",
		"backend_xmin", "backend_xid", "query_id", "xact_start",
		"'idle in transaction%'", "pg_backend_pid()", notSelfFragment(),
	} {
		if !strings.Contains(locksSQL, want) {
			t.Errorf("locksSQL lacks %q", want)
		}
	}
	if strings.Count(locksSQL, "pg_blocking_pids(") != 1 {
		t.Error("locksSQL must call pg_blocking_pids once, on the capped waiter set")
	}
}

func notSelfFragment() string { return "NOT ILIKE '%pg_sage%'" }
