package agentloop

import (
	"strings"
	"unicode/utf8"
)

// FilterClaims is the citation filter: a claim survives only when it
// cites at least one alias, every alias it cites is evidence of the run,
// and ground (when set) accepts it against the cited evidence. Survivors
// cite durable evidence ids (each once, in order); the rest are dropped
// with a reason, never repaired. At most max claims survive (0 means
// DefaultMaxClaims); duplicates (same normalized text) are dropped.
func FilterClaims(claims []Claim, cited map[string]Evidence,
	ground func(text string, cited []Evidence) error, max int) ([]Claim, []DroppedClaim) {
	if len(claims) == 0 {
		return nil, nil
	}
	if max <= 0 {
		max = DefaultMaxClaims
	}
	var kept []Claim
	var dropped []DroppedClaim
	seen := map[string]bool{}
	for _, c := range claims {
		text := oneLine(c.Text)
		ev, ids, reason := checkClaim(text, c.EvidenceIDs, cited)
		if reason == "" && ground != nil && ground(text, ev) != nil {
			reason = DropUngrounded
		}
		norm := strings.ToLower(text)
		switch {
		case reason != "":
		case seen[norm]:
			reason = DropDuplicate
		case len(kept) >= max:
			reason = DropOverLimit
		}
		if reason != "" {
			dropped = append(dropped, DroppedClaim{Text: clip(text, 200), Reason: reason})
			continue
		}
		seen[norm] = true
		kept = append(kept, Claim{Text: text, EvidenceIDs: ids})
	}
	return kept, dropped
}

// checkClaim resolves a claim's aliases, or says why it cannot stand.
func checkClaim(text string, aliases []string, cited map[string]Evidence) ([]Evidence,
	[]string, string) {
	switch {
	case text == "":
		return nil, nil, DropEmpty
	case utf8.RuneCountInString(text) > MaxClaimRunes:
		return nil, nil, DropTooLong
	case len(aliases) == 0:
		return nil, nil, DropUncited
	}
	var ev []Evidence
	var ids []string
	have := map[string]bool{}
	for _, alias := range aliases {
		e, ok := cited[alias]
		if !ok {
			return nil, nil, DropUnknownEvidence
		}
		if !have[e.ID] {
			have[e.ID] = true
			ev, ids = append(ev, e), append(ids, e.ID)
		}
	}
	return ev, ids, ""
}
