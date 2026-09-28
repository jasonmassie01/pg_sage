package sre

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Claim validator (AI-SRE-SPEC §2.3/§5): every claim must cite evidence
// ids in scope, and every number it states must equal a number in the
// evidence that claim cites. Numbers are computed by code and bound as
// evidence; the model may only quote them.

// Claim limits.
const (
	MaxClaims     = 5
	MaxClaimRunes = 1200
)

// Claim is one narrated statement with the evidence it rests on.
type Claim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// EvidenceCatalog maps evidence ids in scope to their text.
type EvidenceCatalog map[string]string

// Claim validation errors.
var (
	ErrNoClaims         = errors.New("no claims")
	ErrTooManyClaims    = errors.New("too many claims")
	ErrInvalidClaim     = errors.New("invalid claim")
	ErrUncitedClaim     = errors.New("claim must cite evidence")
	ErrUnknownEvidence  = errors.New("unknown evidence id")
	ErrDuplicateClaim   = errors.New("duplicate claim")
	ErrUngroundedNumber = errors.New("ungrounded number")
)

var (
	numberPattern = regexp.MustCompile(`\d+(?:\.\d+)?`)
	thousandsSep  = regexp.MustCompile(`(\d),(\d{3})\b`)
	evidenceRef   = regexp.MustCompile(`\b[EPH]\d+\b`)
	spaceCollapse = regexp.MustCompile(`\s+`)
)

// ValidateClaims checks every claim against the evidence catalog.
func ValidateClaims(claims []Claim, ev EvidenceCatalog) error {
	switch {
	case len(claims) == 0:
		return ErrNoClaims
	case len(claims) > MaxClaims:
		return fmt.Errorf("%w: %d > %d", ErrTooManyClaims, len(claims), MaxClaims)
	}
	seen := map[string]bool{}
	for i, c := range claims {
		norm := strings.ToLower(spaceCollapse.ReplaceAllString(strings.TrimSpace(c.Text), " "))
		if seen[norm] {
			return fmt.Errorf("%w: claim %d", ErrDuplicateClaim, i+1)
		}
		seen[norm] = true
		if err := validateClaim(i+1, c, ev); err != nil {
			return err
		}
	}
	return nil
}

func validateClaim(n int, c Claim, ev EvidenceCatalog) error {
	text := strings.TrimSpace(c.Text)
	switch {
	case text == "":
		return fmt.Errorf("%w: claim %d is empty", ErrInvalidClaim, n)
	case utf8.RuneCountInString(text) > MaxClaimRunes:
		return fmt.Errorf("%w: claim %d longer than %d characters", ErrInvalidClaim, n,
			MaxClaimRunes)
	case len(c.EvidenceIDs) == 0:
		return fmt.Errorf("%w: claim %d", ErrUncitedClaim, n)
	}
	known := map[float64]bool{}
	for _, id := range c.EvidenceIDs {
		body, ok := ev[id]
		if !ok || id == "" {
			return fmt.Errorf("%w %q in claim %d", ErrUnknownEvidence, id, n)
		}
		for _, v := range numbers(body) {
			known[v.value] = true
		}
	}
	for _, v := range numbers(evidenceRef.ReplaceAllString(text, "")) {
		if !known[v.value] {
			return fmt.Errorf("%w %s in claim %d", ErrUngroundedNumber, v.token, n)
		}
	}
	return nil
}

type number struct {
	token string
	value float64
}

// numbers extracts the decimal numbers of s ("1,200" is 1200).
func numbers(s string) []number {
	s = thousandsSep.ReplaceAllString(s, "$1$2")
	var out []number
	for _, tok := range numberPattern.FindAllString(s, -1) {
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			out = append(out, number{token: tok, value: v})
		}
	}
	return out
}

// Citations is the union of the claims' evidence ids, in first-seen order.
func Citations(claims []Claim) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range claims {
		for _, id := range c.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
