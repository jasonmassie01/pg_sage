package facts

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Size bounds of a stored proposal.
const (
	maxCitations      = 10
	maxCitationDetail = 500
	maxCitationRef    = 300
	maxRationale      = 2000
	maxProposedBy     = 200
	maxConsumer       = 128
	maxValueText      = 300
)

// typeKinds are the subject kinds each fact type allows.
var typeKinds = map[Type][]Kind{
	TypeAppMigrations: {KindIndex, KindTable, KindSchema},
	TypeTestFixture:   {KindSchema},
	TypeSlotConsumer:  {KindSlot},
	TypeAppendOnly:    {KindTable},
	TypeTableWindow:   {KindTable},
}

// valueKeys are the value fields each fact type accepts; required ones
// are checked by validateValue.
var valueKeys = map[Type]map[string]bool{
	TypeAppMigrations: {"repo": true, "path": true, "note": true},
	TypeTestFixture:   {"note": true},
	TypeSlotConsumer:  {"consumer": true},
	TypeAppendOnly:    {"note": true},
	TypeTableWindow:   {"kind": true, "window": true},
}

// Validate checks a proposal and returns it normalized: the subject in its
// canonical spelling, the value non-nil and every text bounded.
func Validate(p Proposal) (Proposal, error) {
	kinds, ok := typeKinds[p.Type]
	if !ok {
		return Proposal{}, fmt.Errorf("%w: %q", ErrInvalidType, p.Type)
	}
	if !containsKind(kinds, p.Kind) {
		return Proposal{}, fmt.Errorf("%w: %s facts are about %v, not %q", ErrInvalidKind,
			p.Type, kinds, p.Kind)
	}
	pattern, err := ParsePattern(p.Kind, p.Subject)
	if err != nil {
		return Proposal{}, err
	}
	p.Subject = pattern.String()
	if p.Value, err = validateValue(p.Type, p.Value); err != nil {
		return Proposal{}, err
	}
	if err := validateSource(p); err != nil {
		return Proposal{}, err
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return Proposal{}, fmt.Errorf("%w: expires_at is in the past", ErrInvalidValue)
	}
	p.Evidence = boundCitations(p.Evidence)
	p.Rationale = clip(cleanText(p.Rationale), maxRationale)
	p.ProposedBy = clip(cleanText(p.ProposedBy), maxProposedBy)
	return p, nil
}

func containsKind(kinds []Kind, k Kind) bool {
	for _, kind := range kinds {
		if kind == k {
			return true
		}
	}
	return false
}

func validateValue(t Type, value map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(value))
	for k, v := range value {
		if !valueKeys[t][k] {
			return nil, fmt.Errorf("%w: %s facts take no %q", ErrInvalidValue, t, k)
		}
		v = strings.TrimSpace(cleanText(v))
		if len(v) > maxValueText {
			return nil, fmt.Errorf("%w: %s is longer than %d", ErrInvalidValue, k, maxValueText)
		}
		out[k] = v
	}
	switch t {
	case TypeSlotConsumer:
		if c := out["consumer"]; c == "" || len(c) > maxConsumer {
			return nil, fmt.Errorf("%w: a slot fact names its consumer (1-%d characters)",
				ErrInvalidValue, maxConsumer)
		}
	case TypeTableWindow:
		if k := out["kind"]; k != "maintenance" && k != "batch" {
			return nil, fmt.Errorf("%w: a window is maintenance or batch", ErrInvalidValue)
		}
		if _, err := policy.ParseWindow(out["window"]); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
		}
	}
	return out, nil
}

// validateSource requires cited evidence from the model and detectors; an
// operator's declaration is its own evidence.
func validateSource(p Proposal) error {
	switch p.Source {
	case SourceOperator:
		return nil
	case SourceModel, SourceDetector:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidSource, p.Source)
	}
	for _, c := range p.Evidence {
		if strings.TrimSpace(c.Kind) != "" && strings.TrimSpace(c.Ref) != "" {
			return nil
		}
	}
	return fmt.Errorf("%w: %s proposals cite at least one piece of evidence", ErrNoEvidence,
		p.Source)
}

func boundCitations(in []Citation) []Citation {
	out := make([]Citation, 0, min(len(in), maxCitations))
	for _, c := range in {
		if len(out) == maxCitations {
			break
		}
		c.Kind = clip(cleanText(c.Kind), 40)
		c.Ref = clip(cleanText(c.Ref), maxCitationRef)
		c.Detail = clip(cleanText(c.Detail), maxCitationDetail)
		out = append(out, c)
	}
	return out
}

// cleanText replaces control characters with spaces.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// clip cuts s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
