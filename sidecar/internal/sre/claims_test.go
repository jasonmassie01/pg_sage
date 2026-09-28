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
