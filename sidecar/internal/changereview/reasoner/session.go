package reasoner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/sre"
)

// session is one review's model run: the parsed diff and what was offered
// of it, the seed, and everything the tools returned. agentloop runs a
// reply's tool calls one after another on one goroutine, so the session
// needs no lock.
type session struct {
	r       *Reasoner
	in      changereview.Input
	a       changereview.Analysis
	files   []diffscan.File
	offered diffscan.Offered
	seed    []agentloop.Evidence
	touched map[string]bool

	// aliases mirrors agentloop's numbering: the seed is E1..En, then each
	// citable tool result takes the next alias.
	aliases     map[string]string
	evidence    map[string]changereview.Evidence // tool results by durable id
	inspections map[string]*inspection           // by inspect evidence id
	untouched   int
}

// inspection is one inspect_statement result.
type inspection struct {
	sql    string
	origin string
	ins    changereview.Inspection
	byID   map[string]changereview.Evidence // the inspection's own evidence
}

func (r *Reasoner) newSession(in changereview.Input, a changereview.Analysis) *session {
	s := &session{r: r, in: in, a: a, files: diffscan.Parse(in.Diff),
		touched: map[string]bool{}, aliases: map[string]string{},
		evidence: map[string]changereview.Evidence{}, inspections: map[string]*inspection{}}
	s.offered = diffscan.Offer(s.files, max(in.Config.MaxOfferedDiffBytes, 0))
	for _, rel := range a.Expectations.Relations {
		s.touch(rel)
	}
	for _, st := range a.Statements {
		for _, rel := range st.Relations {
			s.touch(rel)
		}
	}
	s.seed = seedEvidence(a)
	for i, e := range s.seed {
		s.aliases[fmt.Sprintf("E%d", i+1)] = e.ID
	}
	return s
}

func (s *session) touch(rel string) {
	if rel = strings.ToLower(strings.TrimSpace(rel)); rel != "" {
		s.touched[rel] = true
	}
}

// isTouched: a relation of the review, schema-qualified or by name.
func (s *session) isTouched(rel string) bool {
	rel = strings.ToLower(strings.TrimSpace(rel))
	if s.touched[rel] {
		return true
	}
	if strings.Contains(rel, ".") {
		return false
	}
	for t := range s.touched {
		if _, name, ok := strings.Cut(t, "."); ok && name == rel {
			return true
		}
	}
	return false
}

// config is the agentloop run of the session.
func (s *session) config(b Bounds) agentloop.Config {
	return agentloop.Config{System: systemPrompt(s.in.Database), Task: s.taskPrompt(),
		Tools: s.tools(), Final: finalTool(), Seed: s.seed, Budget: b,
		Protocol: s.r.d.Protocol, Ground: sre.GroundClaim, MaxClaims: MaxClaims,
		Now: s.r.d.Now}
}

// seedEvidence is the deterministic evidence the model may cite: what the
// findings cite first, then the statements, then the rest, at most
// maxSeed items.
func seedEvidence(a changereview.Analysis) []agentloop.Evidence {
	byID := map[string]changereview.Evidence{}
	for _, e := range a.Evidence {
		byID[e.ID] = e
	}
	var order []string
	seen := map[string]bool{}
	add := func(id string) {
		if _, ok := byID[id]; ok && !seen[id] && len(order) < maxSeed {
			seen[id] = true
			order = append(order, id)
		}
	}
	for _, f := range a.Findings {
		for _, id := range f.EvidenceIDs {
			add(id)
		}
	}
	for _, st := range a.Statements {
		add(st.EvidenceID)
	}
	for _, e := range a.Evidence {
		add(e.ID)
	}
	out := make([]agentloop.Evidence, 0, len(order))
	for _, id := range order {
		e := byID[id]
		out = append(out, agentloop.Evidence{ID: e.ID, Digest: e.Digest,
			Label: e.ID + " " + e.Label, Text: clipRunes(groundText(e), maxSeedRunes)})
	}
	return out
}

// groundText is what claims citing e are checked against: its id, label
// and data.
func groundText(e changereview.Evidence) string {
	return e.ID + " " + e.Label + " " + string(e.Data)
}

// cite keeps a tool result and makes it citable under the next alias;
// label is pg_sage's own text (it is shown outside the data fence).
func (s *session) cite(e changereview.Evidence, label, summary string) agentloop.Output {
	s.evidence[e.ID] = e
	s.aliases[fmt.Sprintf("E%d", len(s.aliases)+1)] = e.ID
	text := summary + "\n" + string(e.Data)
	return agentloop.Output{Status: "ok", Text: text, Evidence: &agentloop.Evidence{
		ID: e.ID, Digest: e.Digest, Label: e.ID + " " + label, Text: groundText(e)}}
}

// resolve is the durable evidence id of an alias the model cited, or the
// argument itself when it already is one.
func (s *session) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	if id, ok := s.aliases[ref]; ok {
		return id
	}
	return ref
}

func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
