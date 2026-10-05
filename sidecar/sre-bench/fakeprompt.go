package srebench

import (
	"fmt"
	"regexp"
	"strings"
)

// What the fake model reads from the investigator's prompt, and the
// adversarial review it builds from it.

type fakeEvidence struct{ alias, probe, text string }

type fakePrompt struct {
	open     []string
	probes   []string // catalog probes that accept empty args
	evidence []fakeEvidence
}

var (
	hypothesisLine = regexp.MustCompile(`^- ([a-z][a-z0-9_]*) \[`)
	evidenceLine   = regexp.MustCompile(`^(E\d+) \[([a-z][a-z0-9_]*) ([a-z_]+)\](.*)$`)
	menuLine       = regexp.MustCompile(`^- ([a-z][a-z0-9_]*): \{\}`)
	numberToken    = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
)

// parseFakePrompt reads the open hypotheses, the evidence aliases and
// the probe menu (if offered) of the investigator's user prompt.
func parseFakePrompt(user string) fakePrompt {
	var p fakePrompt
	section := ""
	for _, line := range strings.Split(user, "\n") {
		switch {
		case strings.HasPrefix(line, "Open hypotheses"):
			section = "open"
		case strings.HasPrefix(line, "Ruled out"), strings.HasPrefix(line, "Evidence"):
			section = "other"
		case strings.HasPrefix(line, "Catalog probes"):
			section = "menu"
		case section == "open":
			if m := hypothesisLine.FindStringSubmatch(line); m != nil {
				p.open = append(p.open, m[1])
			}
		case section == "menu":
			if m := menuLine.FindStringSubmatch(line); m != nil {
				p.probes = append(p.probes, m[1])
			}
		}
		if m := evidenceLine.FindStringSubmatch(line); m != nil {
			p.evidence = append(p.evidence, fakeEvidence{alias: m[1], probe: m[2],
				text: m[4]})
		}
	}
	return p
}

type fakeProbe struct {
	Probe     string         `json:"probe"`
	Args      map[string]any `json:"args"`
	Rationale string         `json:"rationale"`
}

type fakeClaim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type fakeReview struct {
	Ranking   []string    `json:"ranking"`
	NextProbe *fakeProbe  `json:"next_probe,omitempty"`
	Claims    []fakeClaim `json:"claims"`
}

// adversarialReview ranks the open hypotheses in reverse (the graph's
// last first), asks for an offered no-arg probe (picked by seed) and
// narrates up to three claims citing real evidence ids, one quoting a
// number of its evidence.
func adversarialReview(p fakePrompt, seed uint64) fakeReview {
	r := fakeReview{Ranking: make([]string, 0, len(p.open))}
	for i := len(p.open) - 1; i >= 0; i-- {
		r.Ranking = append(r.Ranking, p.open[i])
	}
	if len(p.probes) > 0 {
		r.NextProbe = &fakeProbe{Probe: p.probes[seed%uint64(len(p.probes))],
			Args:      map[string]any{},
			Rationale: "re-reading it tells the graph's last hypothesis from its first"}
	}
	r.Claims = fakeClaims(p.evidence)
	return r
}

// fakeClaims narrates up to three claims citing real evidence ids, one
// quoting a number of its evidence.
func fakeClaims(ev []fakeEvidence) []fakeClaim {
	out := []fakeClaim{}
	for _, e := range ev {
		if len(out) == 3 {
			break
		}
		text := fmt.Sprintf("Evidence %s comes from the %s probe.", e.alias, e.probe)
		if n := numberToken.FindString(e.text); n != "" {
			text = fmt.Sprintf("The %s evidence (%s) reports %s.", e.probe, e.alias, n)
		}
		out = append(out, fakeClaim{Text: text, EvidenceIDs: []string{e.alias}})
	}
	return out
}
