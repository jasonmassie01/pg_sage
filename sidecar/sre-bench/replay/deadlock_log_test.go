package replay

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-07b: a deadlock case may carry the server-log DETAIL and CONTEXT of
// its deadlock and the participants a DBA must see; the replay scores
// participants_named = 1 when the parser names every one of them.

// No concurrent access tests: scoring is a pure function of a frozen case.

const benchDetail = "Process 16010 waits for ShareLock on transaction 90211; " +
	"blocked by process 16020.\n" +
	"Process 16020 waits for ShareLock on transaction 90210; blocked by process 16010.\n" +
	"Process 16010: UPDATE ledger SET balance = balance - 25 WHERE account_id = 7101\n" +
	"Process 16020: UPDATE ledger SET balance = balance + 25 WHERE account_id = 7100"

func deadlockLogCase(participants []any) map[string]any {
	m := validCase()
	m["deadlock_log"] = map[string]any{"detail": benchDetail,
		"context":      `while updating tuple (12,4) in relation "ledger"`,
		"participants": participants}
	return m
}

func expect(pid, blockedBy int, mode string) map[string]any {
	return map[string]any{"pid": pid, "wait_mode": mode, "blocked_by": blockedBy}
}

// CHECK-23: the corpus case lock-deadlock-cycle scores participants_named = 1.
func TestCorpus_DeadlockCycleNamesItsParticipants(t *testing.T) {
	for _, c := range loadCorpus(t) {
		if c.ID != "lock-deadlock-cycle" {
			continue
		}
		named, ok := ParticipantsNamed(c, logwatchParser)
		if !ok || named != 1 {
			t.Fatalf("participants_named = %d (scored %v), want 1", named, ok)
		}
		if len(c.DeadlockLog.Participants) != 2 {
			t.Errorf("expected participants = %+v, want both processes",
				c.DeadlockLog.Participants)
		}
		return
	}
	t.Fatal("lock-deadlock-cycle is not in the corpus")
}

// Happy path and a miss: a wrong mode or blocker scores 0.
func TestParticipantsNamed_ScoresEveryExpectedParticipant(t *testing.T) {
	c, err := parseValid(t, deadlockLogCase([]any{expect(16010, 16020, "ShareLock"),
		expect(16020, 16010, "ShareLock")}))
	if err != nil {
		t.Fatal(err)
	}
	if named, ok := ParticipantsNamed(c, logwatchParser); !ok || named != 1 {
		t.Errorf("participants_named = %d (%v), want 1", named, ok)
	}
	for _, miss := range [][]any{
		{expect(16010, 16020, "ExclusiveLock")},
		{expect(16010, 16099, "ShareLock")},
		{expect(16030, 16010, "ShareLock")},
	} {
		c, err := parseValid(t, deadlockLogCase(miss))
		if err != nil {
			t.Fatal(err)
		}
		if named, ok := ParticipantsNamed(c, logwatchParser); !ok || named != 0 {
			t.Errorf("%v: participants_named = %d (%v), want 0", miss, named, ok)
		}
	}
}

// A participant without a statement line has no fingerprint: not named.
func TestParticipantsNamed_NeedsAFingerprint(t *testing.T) {
	m := deadlockLogCase([]any{expect(16010, 16020, "ShareLock")})
	m["deadlock_log"].(map[string]any)["detail"] = strings.SplitN(benchDetail, "\n", 3)[0] +
		"\n" + strings.SplitN(benchDetail, "\n", 3)[1]
	c, err := parseValid(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if named, ok := ParticipantsNamed(c, logwatchParser); !ok || named != 0 {
		t.Errorf("participants_named = %d (%v), want 0 without fingerprints", named, ok)
	}
}

// Nil/empty: a case without a deadlock log is not scored.
func TestParticipantsNamed_NotScoredWithoutDeadlockLog(t *testing.T) {
	c, err := parseValid(t, validCase())
	if err != nil {
		t.Fatal(err)
	}
	if named, ok := ParticipantsNamed(c, logwatchParser); ok || named != 0 {
		t.Errorf("participants_named = %d (%v), want not scored", named, ok)
	}
}

// Invalid input: an empty DETAIL, no expected participants or a
// non-positive PID fail validation; an unknown field fails the parse.
func TestValidate_RejectsBadDeadlockLog(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"empty detail": func(m map[string]any) {
			m["deadlock_log"].(map[string]any)["detail"] = " "
		},
		"no participants": func(m map[string]any) {
			m["deadlock_log"].(map[string]any)["participants"] = []any{}
		},
		"zero pid": func(m map[string]any) {
			m["deadlock_log"].(map[string]any)["participants"] =
				[]any{expect(0, 16020, "ShareLock")}
		},
		"no mode": func(m map[string]any) {
			m["deadlock_log"].(map[string]any)["participants"] =
				[]any{expect(16010, 16020, "")}
		},
	}
	for name, mutate := range cases {
		m := deadlockLogCase([]any{expect(16010, 16020, "ShareLock")})
		mutate(m)
		c, err := Parse(encode(t, m))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if err := c.Validate(probes.Catalog()); !errors.Is(err, ErrInvalidCase) ||
			!strings.Contains(err.Error(), "deadlock_log") {
			t.Errorf("%s: Validate = %v, want an invalid deadlock_log", name, err)
		}
	}
	m := deadlockLogCase([]any{expect(16010, 16020, "ShareLock")})
	m["deadlock_log"].(map[string]any)["statements"] = "raw"
	if _, err := Parse(encode(t, m)); !errors.Is(err, ErrInvalidCase) {
		t.Errorf("unknown deadlock_log field parsed: %v", err)
	}
}

// The DETAIL is case text: a canary planted there is found.
func TestCaseText_IncludesDeadlockDetail(t *testing.T) {
	c, err := Parse(encode(t, deadlockLogCase([]any{expect(16010, 16020, "ShareLock")})))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(caseText(c), "account_id = 7101") {
		t.Error("caseText leaves out the deadlock DETAIL")
	}
}

// logwatchParser is pg_sage's deadlock log parser, the one the bench
// passes, as a DeadlockParser.
func logwatchParser(detail, context string) []ParsedParticipant {
	parts, _ := logwatch.ParseDeadlockDetail(detail, context)
	out := make([]ParsedParticipant, 0, len(parts))
	for _, p := range parts {
		out = append(out, ParsedParticipant{PID: p.PID, BlockedBy: p.BlockedBy,
			WaitMode: p.WaitMode, Fingerprint: p.Fingerprint})
	}
	return out
}

// Without a parser a deadlock case is scored, and scored 0: a miss, never
// a silent skip.
func TestParticipantsNamed_NoParserScoresZero(t *testing.T) {
	m := deadlockLogCase([]any{expect(16010, 16020, "ShareLock"),
		expect(16020, 16010, "ShareLock")})
	c, err := parseValid(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if named, ok := ParticipantsNamed(c, nil); !ok || named != 0 {
		t.Fatalf("no parser: participants_named = %d (scored %v), want 0 scored", named, ok)
	}
	if named, ok := ParticipantsNamed(c, logwatchParser); !ok || named != 1 {
		t.Fatalf("logwatch parser: participants_named = %d (scored %v), want 1", named, ok)
	}
}
