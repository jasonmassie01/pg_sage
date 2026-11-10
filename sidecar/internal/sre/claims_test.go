package sre

import (
	"errors"
	"strings"
	"testing"
)

// Claim validator (AI-SRE-SPEC §2.3, §5): every claim cites evidence ids
// in scope, and every number in a claim must equal a number in the
// evidence that claim cites. Numbers are computed by code; the model may
// only quote them.

func catalog() EvidenceCatalog {
	return EvidenceCatalog{
		"E2": "Root blocker pid 4242 (idle in transaction) blocks 3 sessions",
		"P1": "lock_graph v1 ok, 2 rows\n[1] waiter_pid=20 blocker_pid=4242 " +
			"blocker_xact_age_s=75.31",
		"H1": "idle_in_tx_holder confidence 0.85 pid 4242",
		"P2": "long_transactions v1 ok, 1 rows\n[1] pid=4242 xact_age_s=1200",
	}
}

func TestValidateClaims_AcceptsGroundedClaims(t *testing.T) {
	claims := []Claim{
		{Text: "Session 4242 is idle in transaction and blocks 3 sessions.",
			EvidenceIDs: []string{"E2"}},
		{Text: "Its transaction has been open 75.31 s (confidence 0.85).",
			EvidenceIDs: []string{"P1", "H1"}},
		{Text: "No numbers here, only a mechanism.", EvidenceIDs: []string{"H1"}},
		{Text: "Open for 1,200 seconds.", EvidenceIDs: []string{"P2"}},
	}
	if err := ValidateClaims(claims, catalog()); err != nil {
		t.Fatalf("grounded claims rejected: %v", err)
	}
}

func TestValidateClaims_NumbersBindToTheClaimsOwnCitations(t *testing.T) {
	// 3 appears in E2 but this claim cites only P1.
	claims := []Claim{{Text: "pid 4242 blocks 3 sessions.",
		EvidenceIDs: []string{"P1"}}}
	err := ValidateClaims(claims, catalog())
	if !errors.Is(err, ErrUngroundedNumber) || !strings.Contains(err.Error(), "3") {
		t.Fatalf("err = %v, want ungrounded number 3", err)
	}
}

func TestValidateClaims_NumbersMustMatchExactly(t *testing.T) {
	cases := map[string]bool{
		"open 75.31 s":   true,
		"open 75.310 s":  true, // same value
		"open 75.3 s":    false,
		"open 75 s":      false,
		"blocks 3.0 now": true,
		"confidence 85%": false, // 0.85 is not 85
		"xid 1200":       true,
		"xid 1,200":      true,
		"xid 12,00":      false,
	}
	ev := EvidenceCatalog{"X": "age=75.31 blocked=3 conf=0.85 xid=1200"}
	for text, ok := range cases {
		err := ValidateClaims([]Claim{{Text: text, EvidenceIDs: []string{"X"}}}, ev)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", text, err, ok)
		}
	}
}

func TestValidateClaims_RejectsMalformedClaims(t *testing.T) {
	long := strings.Repeat("a", MaxClaimRunes+1)
	many := make([]Claim, MaxClaims+1)
	for i := range many {
		many[i] = Claim{Text: "claim " + strings.Repeat("x", i),
			EvidenceIDs: []string{"E2"}}
	}
	cases := map[string]struct {
		claims []Claim
		want   error
	}{
		"none":       {nil, ErrNoClaims},
		"too many":   {many, ErrTooManyClaims},
		"empty text": {[]Claim{{Text: "  ", EvidenceIDs: []string{"E2"}}}, ErrInvalidClaim},
		"too long":   {[]Claim{{Text: long, EvidenceIDs: []string{"E2"}}}, ErrInvalidClaim},
		"uncited":    {[]Claim{{Text: "Blocking."}}, ErrUncitedClaim},
		"unknown id": {[]Claim{{Text: "Blocking.", EvidenceIDs: []string{"E9"}}},
			ErrUnknownEvidence},
		"empty id": {[]Claim{{Text: "Blocking.", EvidenceIDs: []string{""}}},
			ErrUnknownEvidence},
		"duplicate": {[]Claim{{Text: "Blocking.", EvidenceIDs: []string{"E2"}},
			{Text: "blocking. ", EvidenceIDs: []string{"H1"}}}, ErrDuplicateClaim},
		"invented num": {[]Claim{{Text: "pid 777 blocks.", EvidenceIDs: []string{"E2"}}},
			ErrUngroundedNumber},
	}
	for name, c := range cases {
		if err := ValidateClaims(c.claims, catalog()); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if err := ValidateClaims([]Claim{{Text: "x", EvidenceIDs: []string{"E2"}}},
		nil); !errors.Is(err, ErrUnknownEvidence) {
		t.Errorf("nil catalog: err = %v, want unknown evidence", err)
	}
}

func TestClaimCitations_UnionInOrder(t *testing.T) {
	got := Citations([]Claim{{EvidenceIDs: []string{"E2", "P1"}},
		{EvidenceIDs: []string{"P1", "H1"}}})
	if strings.Join(got, ",") != "E2,P1,H1" {
		t.Fatalf("Citations = %v", got)
	}
}

// sre-03: formatting is not evidence. A probe's head line (version, row
// count), its [i] row ordinals and the parts of an RFC 3339 timestamp
// must not ground a number; whole timestamps and row values still do.
func TestValidateClaims_FormatArtifactsGroundNothing(t *testing.T) {
	ev := EvidenceCatalog{"P3": "long_transactions v2 ok, 2 rows\n" +
		"[1] pid=4242 xact_start=2026-11-05T03:12:45Z\n" +
		"[2] pid=5001 xact_start=2026-11-05T03:12:46Z\n(7 more rows not shown)"}
	rejected := map[string]string{
		"12 sessions are waiting.":         "12", // timestamp minute
		"The holder idled for 45 seconds.": "45", // timestamp second
		"3 transactions are open.":         "3",  // timestamp hour
		"It began in 2026.":                "2026",
		"blocks 2 sessions":                "2", // row count and ordinal [2]
		"1 transaction is idle.":           "1", // ordinal [1]
		"7 more sessions wait.":            "7", // omitted-row notice
	}
	for text, num := range rejected {
		err := ValidateClaims([]Claim{{Text: text, EvidenceIDs: []string{"P3"}}}, ev)
		if !errors.Is(err, ErrUngroundedNumber) || !strings.Contains(err.Error(), num) {
			t.Errorf("%q: err = %v, want ungrounded number %s", text, err, num)
		}
	}
	for _, text := range []string{
		"pid 4242 began its transaction at 2026-11-05T03:12:45Z.",
		"pid 5001 started at 2026-11-05T04:12:46+01:00.", // the same instant
	} {
		if err := ValidateClaims([]Claim{{Text: text, EvidenceIDs: []string{"P3"}}},
			ev); err != nil {
			t.Errorf("%q rejected: %v", text, err)
		}
	}
}

func TestValidateClaims_RowCountOfP1DoesNotGround(t *testing.T) {
	// P1's only 2 is its "2 rows" head (and nothing else in the row).
	err := ValidateClaims([]Claim{{Text: "pid 4242 blocks 2 sessions.",
		EvidenceIDs: []string{"P1"}}}, catalog())
	if !errors.Is(err, ErrUngroundedNumber) || !strings.Contains(err.Error(), " 2 ") {
		t.Fatalf("err = %v, want ungrounded number 2", err)
	}
}

func TestValidateClaims_IntegersCompareExactlyWithSign(t *testing.T) {
	ev := EvidenceCatalog{"S": "plan_regression v1 ok, 1 rows\n" +
		"[1] queryid=-8123456789012345678 big=20000000000000000000001 calls=1,200,300"}
	cases := map[string]bool{
		"queryid -8123456789012345678 regressed": true,
		"queryid -8123456789012345677 regressed": false, // off by one at ~8e18
		"queryid -8123456789012345679 regressed": false,
		"queryid 8123456789012345678 regressed":  false, // sign flipped
		"20000000000000000000001 bytes":          true,
		"20000000000000000000000 bytes":          false, // beyond int64, off by one
		"1,200,300 calls":                        true,
		"1200300 calls":                          true,
		"1,200 calls":                            false,
	}
	for text, ok := range cases {
		err := ValidateClaims([]Claim{{Text: text, EvidenceIDs: []string{"S"}}}, ev)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", text, err, ok)
		}
	}
}

func TestValidateClaims_MinusIsASignOnlyAfterADelimiter(t *testing.T) {
	ev := EvidenceCatalog{"R": "range 10-20 delta=-5 (-7) off:-9"}
	cases := map[string]bool{
		"between 10 and 20":    true,
		"fell to -20":          false, // 10-20 is a range, not -20
		"delta -5":             true,
		"delta 5":              false, // the evidence says -5
		"(-7) and -9":          true,
		"7 and 9":              false,
		"pages 10-20 were hot": true, // the claim's range is unsigned too
	}
	for text, ok := range cases {
		err := ValidateClaims([]Claim{{Text: text, EvidenceIDs: []string{"R"}}}, ev)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", text, err, ok)
		}
	}
}

func TestValidateClaims_EmptyEvidenceGroundsNoNumber(t *testing.T) {
	ev := EvidenceCatalog{"Z": "", "H": "lock_graph v1 empty, 0 rows"}
	for _, id := range []string{"Z", "H"} {
		err := ValidateClaims([]Claim{{Text: "0 waiters", EvidenceIDs: []string{id}}}, ev)
		if !errors.Is(err, ErrUngroundedNumber) {
			t.Errorf("%s: err = %v, want ungrounded number", id, err)
		}
		if err := ValidateClaims([]Claim{{Text: "No waiters.", EvidenceIDs: []string{id}}},
			ev); err != nil {
			t.Errorf("%s: numberless claim rejected: %v", id, err)
		}
	}
}

// A JSON array (Ask Sage evidence is JSON) keeps its signs: before the fix
// "[-5,-7]" read as 5 and 7, so a sign-flipped claim was grounded.
func TestValidateClaims_JSONArrayMinusIsASign(t *testing.T) {
	ev := EvidenceCatalog{"J": `{"deltas":[-5,-7],"range":"10-20"}`}
	cases := map[string]bool{
		"deltas -5 and -7":  true,
		"delta 5":           false,
		"delta 7":           false,
		"fell to -20":       false,
		"between 10 and 20": true,
	}
	for text, ok := range cases {
		err := ValidateClaims([]Claim{{Text: text, EvidenceIDs: []string{"J"}}}, ev)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", text, err, ok)
		}
	}
}
