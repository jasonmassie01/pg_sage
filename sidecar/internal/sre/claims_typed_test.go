package sre

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// sre-03: the investigator grounds a claim against the typed values of
// the reads it cites (agentloop.Evidence.Values) when they are present,
// and against the formatted text, with its artifacts removed, otherwise.
// No concurrency test: grounding is a pure function of its arguments.

func groundsOK(t *testing.T, text string, ev ...agentloop.Evidence) {
	t.Helper()
	if err := groundClaim(text, ev); err != nil {
		t.Errorf("%q rejected: %v", text, err)
	}
}

func groundsNot(t *testing.T, text, num string, ev ...agentloop.Evidence) {
	t.Helper()
	err := groundClaim(text, ev)
	if !errors.Is(err, ErrUngroundedNumber) || !strings.Contains(err.Error(), num) {
		t.Errorf("%q: err = %v, want ungrounded number %s", text, err, num)
	}
}

func TestGroundClaim_TypedQueryIDsCompareExactly(t *testing.T) {
	ev := agentloop.Evidence{ID: "q", Text: "plan_regression v1 ok, 1 rows",
		Values: []string{"-8123456789012345678", "1000000000000000000"}}
	groundsOK(t, "queryid -8123456789012345678 regressed.", ev)
	groundsOK(t, "queryid 1000000000000000000 regressed.", ev)
	groundsNot(t, "queryid 1000000000000000001 regressed.", "1000000000000000001", ev)
	groundsNot(t, "queryid 999999999999999999 regressed.", "999999999999999999", ev)
	groundsNot(t, "queryid 8123456789012345678 regressed.", "8123456789012345678", ev)
	groundsNot(t, "queryid -1000000000000000000 regressed.", "-1000000000000000000", ev)
}

func TestGroundClaim_TypedDecimalsAndThousands(t *testing.T) {
	ev := agentloop.Evidence{ID: "l", Values: []string{"75.31", "1200", "0.85"}}
	groundsOK(t, "Open 75.31 s.", ev)
	groundsOK(t, "Open 75.310 s.", ev) // the same value at the evidence precision
	groundsOK(t, "Open for 1,200 seconds (confidence 0.85).", ev)
	groundsNot(t, "Open 75.3 s.", "75.3", ev)
	groundsNot(t, "Open 75 s.", "75", ev)
	groundsNot(t, "Confidence 85%.", "85", ev)
}

func TestGroundClaim_ValuesReplaceTheFormattedText(t *testing.T) {
	ev := agentloop.Evidence{ID: "p", Values: []string{"4242", "2026-11-05T03:12:45Z"},
		Text: "lock_graph v1 ok, 2 rows\n[1] blocker_pid=4242 pid=777 " +
			"since=2026-11-05T03:12:45Z"}
	groundsOK(t, "pid 4242 has held its lock since 2026-11-05T03:12:45Z.", ev)
	groundsNot(t, "pid 777 waits.", "777", ev) // only in the text
	groundsNot(t, "12 sessions wait.", "12", ev)
	groundsNot(t, "45 sessions wait.", "45", ev)
	groundsNot(t, "blocks 2 sessions", "2", ev)
	groundsNot(t, "lock_graph v1 says so.", "1", ev)
}

func TestGroundClaim_TextFallbackStripsArtifacts(t *testing.T) {
	ev := agentloop.Evidence{ID: "t", Text: "long_transactions v1 ok, 2 rows\n" +
		"[1] pid=4242 xact_start=2026-11-05T03:12:45Z\n[2] pid=-1 state=idle"}
	groundsOK(t, "pid 4242 and pid -1 are open.", ev)
	groundsNot(t, "2 transactions are open.", "2", ev)
	groundsNot(t, "Started at 03:12.", "3", ev)
	groundsNot(t, "pid 1 is open.", "1", ev) // the evidence says -1
}

func TestGroundClaim_StringValuesGroundTheirOwnDigits(t *testing.T) {
	ev := agentloop.Evidence{ID: "s", Values: []string{"public.orders_2024", "10-20"}}
	groundsOK(t, "orders_2024 is the hot relation.", ev)
	groundsOK(t, "Pages 10 to 20.", ev)
	groundsNot(t, "Pages fell by -20.", "-20", ev)
}

func TestGroundClaim_NilAndEmptyEvidenceGroundNoNumber(t *testing.T) {
	for name, ev := range map[string]agentloop.Evidence{
		"zero":         {},
		"empty values": {ID: "e", Values: []string{}},
		"blank value":  {ID: "b", Values: []string{""}, Text: "3"},
	} {
		if err := groundClaim("3 sessions wait.", []agentloop.Evidence{ev}); !errors.Is(err,
			ErrUngroundedNumber) {
			t.Errorf("%s: err = %v, want ungrounded number", name, err)
		}
		if err := groundClaim("Sessions wait.", []agentloop.Evidence{ev}); err != nil {
			t.Errorf("%s: numberless claim rejected: %v", name, err)
		}
	}
	if err := groundClaim("3 sessions wait.", nil); !errors.Is(err, ErrUncitedClaim) {
		t.Errorf("no evidence: err = %v, want uncited", err)
	}
	if err := groundClaim("  ", []agentloop.Evidence{{ID: "x"}}); !errors.Is(err,
		ErrInvalidClaim) {
		t.Errorf("blank claim: err = %v, want invalid claim", err)
	}
}

func TestGroundClaim_NumbersBindToTheCitedEvidenceOnly(t *testing.T) {
	a := agentloop.Evidence{ID: "a", Values: []string{"4242"}}
	b := agentloop.Evidence{ID: "b", Values: []string{"3"}}
	groundsOK(t, "pid 4242 blocks 3 sessions.", a, b)
	groundsNot(t, "pid 4242 blocks 3 sessions.", "3", a)
}
