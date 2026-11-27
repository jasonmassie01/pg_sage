package logwatch

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// rca-07b: a deadlock DETAIL names, per process, the lock it waited for,
// who blocked it and (in the server log) the statement it ran.

const (
	twoProcDetail = "Process 4242 waits for ShareLock on transaction 745; " +
		"blocked by process 4243.\n" +
		"Process 4243 waits for ShareLock on transaction 744; blocked by process 4242.\n" +
		"Process 4242: UPDATE accounts SET v = v + 1 WHERE id = 2\n" +
		"Process 4243: UPDATE orders SET v = v + 1 WHERE id = 1"
	accountsContext = `while updating tuple (0,2) in relation "accounts"`
)

var hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)

// No concurrent access tests here: ParseDeadlockDetail is a pure function
// of its arguments (the classifier race test is in
// classifier_deadlock_test.go).

// Happy path: two processes on transaction locks give two participants
// with modes, objects, blocked-by and two distinct fingerprints.
func TestParseDeadlockDetail_TwoTransactionLocks(t *testing.T) {
	parts, unparsed := ParseDeadlockDetail(twoProcDetail, accountsContext)
	if unparsed != 0 || len(parts) != 2 {
		t.Fatalf("participants = %+v, unparsed = %d; want 2, 0", parts, unparsed)
	}
	want := []DeadlockParticipant{
		{PID: 4242, WaitMode: "ShareLock", WaitObject: "transaction 745", BlockedBy: 4243,
			Relation: "accounts"},
		{PID: 4243, WaitMode: "ShareLock", WaitObject: "transaction 744", BlockedBy: 4242},
	}
	for i, w := range want {
		p := parts[i]
		if p.PID != w.PID || p.WaitMode != w.WaitMode || p.WaitObject != w.WaitObject ||
			p.BlockedBy != w.BlockedBy || p.Relation != w.Relation {
			t.Errorf("participant %d = %+v, want %+v", i, p, w)
		}
		if !hex16.MatchString(p.Fingerprint) {
			t.Errorf("participant %d fingerprint %q is not 16 hex characters", i, p.Fingerprint)
		}
	}
	if parts[0].Fingerprint == parts[1].Fingerprint {
		t.Errorf("both statements fingerprint to %s", parts[0].Fingerprint)
	}
	if got := parts[0].Statement; got != "UPDATE accounts SET v = v + ? WHERE id = ?" {
		t.Errorf("statement = %q, want the normalized redacted text", got)
	}
}

// The relation name from CONTEXT belongs to the detecting backend (the
// first line); the OID in every wait object is kept.
func TestParseDeadlockDetail_RelationLockKeepsOIDAndNamesContext(t *testing.T) {
	detail := "Process 10 waits for ExclusiveLock on tuple (0,1) of relation 16384 " +
		"of database 5; blocked by process 11.\n" +
		"Process 11 waits for AccessExclusiveLock on relation 16390 of database 5; " +
		"blocked by process 10."
	parts, unparsed := ParseDeadlockDetail(detail, `while locking tuple (0,1) in relation "Orders"`)
	if unparsed != 0 || len(parts) != 2 {
		t.Fatalf("participants = %+v, unparsed = %d", parts, unparsed)
	}
	if parts[0].WaitObject != "tuple (0,1) of relation 16384 of database 5" ||
		parts[0].Relation != "Orders" {
		t.Errorf("first participant = %+v", parts[0])
	}
	if parts[1].WaitObject != "relation 16390 of database 5" || parts[1].Relation != "" ||
		parts[1].WaitMode != "AccessExclusiveLock" {
		t.Errorf("second participant = %+v", parts[1])
	}
	if parts[0].Fingerprint != "" || parts[0].Statement != "" {
		t.Errorf("no statement line, yet fingerprint %q statement %q",
			parts[0].Fingerprint, parts[0].Statement)
	}
	parts, _ = ParseDeadlockDetail(detail, "")
	if parts[0].Relation != "" {
		t.Errorf("no CONTEXT, yet relation %q", parts[0].Relation)
	}
}

func waitLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "Process %d waits for ShareLock on transaction %d; "+
			"blocked by process %d.\n", 100+i, 900+i, 100+(i+1)%n)
	}
	return b.String()
}

// Boundary: 11 processes give 10 participants; the eleventh is not
// counted as unparsed (it parsed, the cap dropped it).
func TestParseDeadlockDetail_CapsAtTenParticipants(t *testing.T) {
	parts, unparsed := ParseDeadlockDetail(waitLines(11), "")
	if len(parts) != 10 || unparsed != 0 {
		t.Fatalf("participants = %d, unparsed = %d; want 10, 0", len(parts), unparsed)
	}
	if parts[9].PID != 109 {
		t.Errorf("tenth participant pid = %d, want 109", parts[9].PID)
	}
	parts, _ = ParseDeadlockDetail(waitLines(10), "")
	if len(parts) != 10 {
		t.Errorf("10 processes give %d participants, want 10", len(parts))
	}
}

// Boundary: a 2,001-byte statement is clipped to 2,000 bytes before it is
// fingerprinted, so its last byte does not change the fingerprint.
func TestParseDeadlockDetail_ClipsStatementBeforeFingerprint(t *testing.T) {
	head := "SELECT " + strings.Repeat("c", 2000-len("SELECT "))
	a := "Process 1: " + head + "x"
	b := "Process 1: " + head + "y"
	pa, _ := ParseDeadlockDetail(a, "")
	pb, _ := ParseDeadlockDetail(b, "")
	if len(pa) != 1 || len(pb) != 1 {
		t.Fatalf("participants = %d, %d; want 1, 1", len(pa), len(pb))
	}
	if pa[0].Fingerprint == "" || pa[0].Fingerprint != pb[0].Fingerprint {
		t.Errorf("fingerprints %q and %q differ past byte 2,000", pa[0].Fingerprint,
			pb[0].Fingerprint)
	}
	short := "Process 1: " + head[:len(head)-1] + "z"
	ps, _ := ParseDeadlockDetail(short, "")
	if ps[0].Fingerprint == pa[0].Fingerprint {
		t.Error("a change inside the first 2,000 bytes kept the fingerprint")
	}
}

// Boundary: a 301-rune redacted text is clipped to 300 runes, never
// inside a multi-byte character.
func TestParseDeadlockDetail_ClipsTextTo300Runes(t *testing.T) {
	stmt := "SELECT " + strings.Repeat("ü", 294) // 301 runes
	parts, _ := ParseDeadlockDetail("Process 7: "+stmt, "")
	got := parts[0].Statement
	if n := utf8.RuneCountInString(got); n != 300 || !utf8.ValidString(got) {
		t.Errorf("statement has %d runes (valid %v), want 300", n, utf8.ValidString(got))
	}
	if !strings.HasPrefix(stmt, got) {
		t.Errorf("clipped text %q is not a prefix of the statement", got)
	}
	parts, _ = ParseDeadlockDetail("Process 7: "+stmt[:len(stmt)-2], "")
	if n := utf8.RuneCountInString(parts[0].Statement); n != 300 {
		t.Errorf("a 300-rune statement became %d runes", n)
	}
}

// Invalid input: literals never survive in the text, and the same shape
// with other values has the same fingerprint.
func TestParseDeadlockDetail_RedactsLiterals(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"UPDATE users SET email = 'alice@example.com' WHERE card = 4111111111111111",
			"UPDATE users SET email = 'bob@example.org' WHERE card = 5500000000000004"},
		{"INSERT INTO cards VALUES ('alice@example.com', 4111111111111111)",
			"INSERT INTO cards VALUES ('bob@example.org', 5500000000000004)"},
		{"SELECT f($$alice@example.com$$, -4111111111111111.5e3)",
			"SELECT f($$bob@example.org$$, -5500000000000004.5e3)"},
	} {
		pa, _ := ParseDeadlockDetail("Process 1: "+tc.a, "")
		pb, _ := ParseDeadlockDetail("Process 1: "+tc.b, "")
		if len(pa) != 1 || pa[0].Fingerprint == "" {
			t.Fatalf("%q: participants %+v, want one with a fingerprint", tc.a, pa)
		}
		for _, leak := range []string{"alice", "example.com", "4111111111111111", "411111"} {
			if strings.Contains(pa[0].Statement, leak) {
				t.Errorf("redacted text %q still holds %q", pa[0].Statement, leak)
			}
		}
		if pa[0].Fingerprint != pb[0].Fingerprint {
			t.Errorf("%q and %q: fingerprints %s, %s; want the same shape id", tc.a, tc.b,
				pa[0].Fingerprint, pb[0].Fingerprint)
		}
	}
}

// Identifiers with digits and $n parameters are shape, not values.
func TestParseDeadlockDetail_KeepsIdentifiersAndParams(t *testing.T) {
	parts, _ := ParseDeadlockDetail(`Process 1: UPDATE t1 SET "col 2" = $1 WHERE id = 42`, "")
	if got := parts[0].Statement; got != `UPDATE t1 SET "col 2" = $1 WHERE id = ?` {
		t.Errorf("statement = %q", got)
	}
}

// Whitespace never changes the fingerprint: a multi-line statement in the
// DETAIL continues on the next lines and is collapsed.
func TestParseDeadlockDetail_MultiLineStatementAndWhitespace(t *testing.T) {
	multi := "Process 3 waits for ShareLock on transaction 9; blocked by process 4.\n" +
		"Process 3: UPDATE t\n   SET v = 1\n\tWHERE id = 7"
	one := "Process 3: UPDATE t SET v = 1 WHERE id = 7"
	pm, um := ParseDeadlockDetail(multi, "")
	po, _ := ParseDeadlockDetail(one, "")
	if um != 0 || len(pm) != 1 {
		t.Fatalf("participants = %+v, unparsed = %d", pm, um)
	}
	if pm[0].Fingerprint != po[0].Fingerprint {
		t.Errorf("multi-line fingerprint %s != one-line %s", pm[0].Fingerprint, po[0].Fingerprint)
	}
	if pm[0].Statement != "UPDATE t SET v = ? WHERE id = ?" || pm[0].BlockedBy != 4 {
		t.Errorf("participant = %+v", pm[0])
	}
}

// Invalid input: a German server's DETAIL gives no participants and
// counts every line unparsed.
func TestParseDeadlockDetail_GermanDetailGivesNone(t *testing.T) {
	de := "Prozess 4242 wartet auf ShareLock-Sperre auf Transaktion 745; " +
		"blockiert von Prozess 4243.\n" +
		"Prozess 4243 wartet auf ShareLock-Sperre auf Transaktion 744; " +
		"blockiert von Prozess 4242.\n" +
		"Prozess 4242: UPDATE accounts SET v = v + 1 WHERE id = 2\n" +
		"Prozess 4243: UPDATE orders SET v = v + 1 WHERE id = 1"
	parts, unparsed := ParseDeadlockDetail(de,
		`beim Aktualisieren von Tupel (0,2) in Relation "accounts"`)
	if len(parts) != 0 || unparsed != 4 {
		t.Errorf("participants = %+v, unparsed = %d; want none, 4", parts, unparsed)
	}
}

// Nil/empty: no DETAIL, no participants, nothing unparsed.
func TestParseDeadlockDetail_Empty(t *testing.T) {
	for _, d := range []string{"", "\n", "  \n\t"} {
		parts, unparsed := ParseDeadlockDetail(d, accountsContext)
		if parts != nil || unparsed != 0 {
			t.Errorf("%q: participants = %+v, unparsed = %d; want nil, 0", d, parts, unparsed)
		}
	}
}

// Error propagation: a malformed line in the middle keeps what parsed
// around it and counts only itself.
func TestParseDeadlockDetail_MalformedMiddleLine(t *testing.T) {
	detail := "Process 1 waits for ShareLock on transaction 9; blocked by process 2.\n" +
		"Process two waits for something odd\n" +
		"Process 2 waits for ShareLock on transaction 8; blocked by process 1.\n" +
		"Process 1: SELECT 1"
	parts, unparsed := ParseDeadlockDetail(detail, "")
	if len(parts) != 2 || unparsed != 1 {
		t.Fatalf("participants = %+v, unparsed = %d; want 2, 1", parts, unparsed)
	}
	if parts[0].PID != 1 || parts[1].PID != 2 || parts[0].Fingerprint == "" {
		t.Errorf("participants = %+v", parts)
	}
}

// A statement line for a process without a wait line still names it.
func TestParseDeadlockDetail_StatementOnlyProcess(t *testing.T) {
	parts, unparsed := ParseDeadlockDetail("Process 55: DELETE FROM q WHERE id = 3", "")
	if unparsed != 0 || len(parts) != 1 || parts[0].PID != 55 || parts[0].WaitMode != "" ||
		parts[0].Fingerprint == "" {
		t.Errorf("participants = %+v, unparsed = %d", parts, unparsed)
	}
}

func TestStatementFingerprint_EmptyStatement(t *testing.T) {
	if fp, text := statementFingerprint("   "); fp != "" || text != "" {
		t.Errorf("blank statement fingerprint %q text %q, want empty", fp, text)
	}
}

// Regression on real output: the server-log DETAIL and CONTEXT of a
// deadlock on PostgreSQL 14 and 17 (both identical in shape; captured
// from the stderr log, whose continuation lines are tab-indented).
func TestParseDeadlockDetail_RealServerLog(t *testing.T) {
	detail := "Process 103 waits for ShareLock on transaction 1348; blocked by process 102.\n" +
		"\tProcess 102 waits for ShareLock on transaction 1349; blocked by process 103.\n" +
		"\tProcess 103: UPDATE rca07b_cards SET card = 5500000000000004 WHERE id = 1 " +
		"AND email <> 'bob@example.org'\n" +
		"\tProcess 102: UPDATE rca07b_cards SET email = 'alice@example.com' WHERE id = 2 " +
		"AND card <> 4111111111111111"
	parts, unparsed := ParseDeadlockDetail(detail,
		`while updating tuple (0,1) in relation "rca07b_cards"`)
	if unparsed != 0 || len(parts) != 2 {
		t.Fatalf("participants = %+v, unparsed = %d", parts, unparsed)
	}
	if parts[0].PID != 103 || parts[0].BlockedBy != 102 || parts[0].Relation != "rca07b_cards" ||
		parts[1].PID != 102 || parts[1].BlockedBy != 103 || parts[1].Relation != "" {
		t.Errorf("participants = %+v", parts)
	}
	if parts[0].Statement != "UPDATE rca07b_cards SET card = ? WHERE id = ? AND email <> '?'" {
		t.Errorf("statement = %q", parts[0].Statement)
	}
	if parts[0].Fingerprint == parts[1].Fingerprint {
		t.Error("two statement shapes share a fingerprint")
	}
}

// Invalid input: comments are free text (a name, a request id) and never
// reach the text or split one shape into several fingerprints.
func TestParseDeadlockDetail_StripsComments(t *testing.T) {
	plain, _ := ParseDeadlockDetail("Process 1: UPDATE t SET v = 1 WHERE id = 7", "")
	commented, _ := ParseDeadlockDetail("Process 1: /* customer alice req=ab12cd */ "+
		"UPDATE t SET v = 1 -- ticket bob\n WHERE id = 7", "")
	if len(commented) != 1 || commented[0].Statement != "UPDATE t SET v = ? WHERE id = ?" {
		t.Fatalf("participants = %+v, want the statement without its comments", commented)
	}
	if commented[0].Fingerprint != plain[0].Fingerprint {
		t.Errorf("fingerprints %s (commented), %s (plain); want the same shape id",
			commented[0].Fingerprint, plain[0].Fingerprint)
	}
}
