package sre

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// The evidence a model turn sees and may cite. Each stored evidence row
// gets a short alias (E1, E2, ... in store order, so aliases stay stable
// as a model-proposed probe appends evidence). Its text is the probe and
// status line plus the facts the deterministic diagnosis bound to it:
// numbers there were computed by code, and a claim may only quote them.
// Everything is redacted before it can reach a provider.

// maxEntryRunes bounds one evidence entry's text in the prompt.
const maxEntryRunes = 1200

type evidenceEntry struct {
	alias  string
	id     UUID
	probe  string
	status string
	reason string
	text   string
	stale  bool
}

type evidenceCatalog []evidenceEntry

func buildEvidenceCatalog(d causal.Diagnosis, ev []Evidence) evidenceCatalog {
	facts := factsByEvidence(d)
	out := make(evidenceCatalog, 0, len(ev))
	for i, e := range ev {
		var head struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(e.Payload, &head); err != nil {
			head.Status, head.Reason = "unreadable", "payload does not decode"
		}
		entry := evidenceEntry{alias: fmt.Sprintf("E%d", i+1), id: e.ID, probe: e.ProbeID,
			status: head.Status, reason: RedactText(head.Reason), stale: !e.VerifyHash()}
		text := entry.probe + " " + entry.status
		if entry.reason != "" {
			text += " " + entry.reason
		}
		if fs := facts[string(e.ID)]; len(fs) > 0 {
			text += ": " + strings.Join(fs, "; ")
		}
		entry.text = truncateRunes(RedactText(text), maxEntryRunes)
		out = append(out, entry)
	}
	return out
}

// factsByEvidence groups the diagnosis' fact texts by the evidence id
// they cite, once each, in diagnosis order.
func factsByEvidence(d causal.Diagnosis) map[string][]string {
	var hs []causal.Hypothesis
	if d.Root != nil {
		hs = append(hs, *d.Root)
	}
	hs = append(append(append(hs, d.Contributing...), d.Alternatives...), d.RuledOut...)
	var all []causal.Fact
	for _, h := range hs {
		all = append(append(all, h.Support...), h.Contradict...)
	}
	all = append(all, d.Observed...)
	out := map[string][]string{}
	seen := map[string]bool{}
	for _, f := range all {
		key := f.EvidenceID + "\x00" + f.Text
		if !seen[key] {
			seen[key] = true
			out[f.EvidenceID] = append(out[f.EvidenceID], f.Text)
		}
	}
	return out
}

func (c evidenceCatalog) byAlias(alias string) (evidenceEntry, bool) {
	for _, e := range c {
		if e.alias == alias {
			return e, true
		}
	}
	return evidenceEntry{}, false
}

func (c evidenceCatalog) byID(id UUID) (evidenceEntry, bool) {
	for _, e := range c {
		if e.id == id {
			return e, true
		}
	}
	return evidenceEntry{}, false
}

// resolve maps an alias to its stored evidence id.
func (c evidenceCatalog) resolve(alias string) (UUID, bool) {
	e, ok := c.byAlias(alias)
	return e.id, ok
}

// texts is the claim validator's view: alias -> evidence text.
func (c evidenceCatalog) texts() EvidenceCatalog {
	out := EvidenceCatalog{}
	for _, e := range c {
		out[e.alias] = e.text
	}
	return out
}
