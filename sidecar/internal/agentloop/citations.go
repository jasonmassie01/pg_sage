package agentloop

import (
	"strings"
	"unicode/utf8"
)

// FilterClaims is the citation filter: a claim survives only when it
// cites at least one alias, every alias it cites is evidence of the run,
// and ground (when set) accepts it against the cited evidence (every
// cited read, even two that share a durable id). Survivors cite durable
// evidence ids and the aliases they resolved (each once, in order); the
// rest are dropped with a reason, never repaired, and aliases a model
// writes into a claim itself are ignored. At most max claims survive (0 means
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
		ev, ids, aliases, reason := checkClaim(text, c.EvidenceIDs, cited)
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
		kept = append(kept, Claim{Text: text, EvidenceIDs: ids, Aliases: aliases})
	}
	return kept, dropped
}

// checkClaim resolves a claim's aliases, or says why it cannot stand:
// the evidence of each alias (once per alias), the durable ids (once per
// id) and the aliases (once each), all in cited order.
func checkClaim(text string, refs []string, cited map[string]Evidence) ([]Evidence,
	[]string, []string, string) {
	switch {
	case text == "":
		return nil, nil, nil, DropEmpty
	case utf8.RuneCountInString(text) > MaxClaimRunes:
		return nil, nil, nil, DropTooLong
	case len(refs) == 0:
		return nil, nil, nil, DropUncited
	}
	var ev []Evidence
	var ids, aliases []string
	haveAlias, haveID := map[string]bool{}, map[string]bool{}
	for _, alias := range refs {
		e, ok := cited[alias]
		if !ok {
			return nil, nil, nil, DropUnknownEvidence
		}
		if haveAlias[alias] {
			continue
		}
		haveAlias[alias] = true
		ev, aliases = append(ev, e), append(aliases, alias)
		if !haveID[e.ID] {
			haveID[e.ID] = true
			ids = append(ids, e.ID)
		}
	}
	return ev, ids, aliases, ""
}
