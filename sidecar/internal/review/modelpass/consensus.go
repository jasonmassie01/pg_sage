package modelpass

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// merged is the runs' combined answer, before it meets the candidates.
type merged struct {
	kept       []keptItem
	votes      map[string]int
	links      []report.Link
	hypotheses []report.Hypothesis
	reports    map[string]reportItem
	rejected   []report.Rejected
	dropped    []agentloop.DroppedClaim
	refuted    map[string]string
}

// keptItem is a kept candidate: its median rank, votes and the first
// keeping run's attack and claims.
type keptItem struct {
	id     string
	median int
	votes  int
	first  rankItem
}

// majority keeps a candidate that need runs keep, at its median rank
// (the lower median); a hypothesis or link survives when need runs give
// the same normalized key. Report text is the first run's that has one.
func majority(answers []checked, need int) merged {
	m := merged{votes: map[string]int{}, reports: map[string]reportItem{},
		refuted: map[string]string{}}
	ranks := map[string][]int{}
	first := map[string]rankItem{}
	var order []string
	for _, a := range answers {
		m.rejected = append(m.rejected, a.rejected...)
		m.dropped = append(m.dropped, a.dropped...)
		for _, it := range a.ranking {
			if it.Decision != report.DecisionKeep {
				continue
			}
			if _, ok := first[it.CandidateID]; !ok {
				first[it.CandidateID] = it
				order = append(order, it.CandidateID)
			}
			ranks[it.CandidateID] = append(ranks[it.CandidateID], it.Rank)
			m.votes[it.CandidateID]++
		}
		for _, r := range a.reports {
			if _, ok := m.reports[r.Dimension]; !ok {
				m.reports[r.Dimension] = r
			}
		}
	}
	for _, id := range order {
		if v := m.votes[id]; v >= need {
			m.kept = append(m.kept, keptItem{id: id, median: median(ranks[id]), votes: v,
				first: first[id]})
		}
	}
	m.hypotheses = hypothesesOf(answers, need)
	m.links = linksOf(answers, need)
	return m
}

func median(xs []int) int {
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[(len(s)-1)/2]
}

func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func hypothesesOf(answers []checked, need int) []report.Hypothesis {
	votes := map[string]int{}
	first := map[string]hypothesis{}
	var order []string
	for _, a := range answers {
		seen := map[string]bool{}
		for _, h := range a.hypotheses {
			key := norm(h.Kind) + "\x00" + norm(h.Target)
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, ok := first[key]; !ok {
				first[key], order = h, append(order, key)
			}
			votes[key]++
		}
	}
	var out []report.Hypothesis
	for _, k := range order {
		if votes[k] >= need {
			h := first[k]
			out = append(out, report.Hypothesis{Title: h.Title, Kind: h.Kind, Target: h.Target,
				Rationale: h.Rationale, Claims: h.Claims, Votes: votes[k]})
		}
	}
	return out
}

func linksOf(answers []checked, need int) []report.Link {
	votes := map[string]int{}
	first := map[string]linkItem{}
	var order []string
	for _, a := range answers {
		seen := map[string]bool{}
		for _, l := range a.links {
			x, y := l.A, l.B
			if y < x {
				x, y = y, x
			}
			key := x + "\x00" + y + "\x00" + l.Relation
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, ok := first[key]; !ok {
				first[key], order = l, append(order, key)
			}
			votes[key]++
		}
	}
	var out []report.Link
	for _, k := range order {
		if votes[k] >= need {
			l := first[k]
			out = append(out, report.Link{A: l.A, B: l.B, Relation: l.Relation,
				Claims: l.Claims, Votes: votes[k]})
		}
	}
	return out
}

// compose fills base with the merged answer: the kept candidates by
// median rank (then votes, then the deterministic score, then id), the
// rest dropped with their votes, and the model's text where it has one.
func (p *pass) compose(base report.Report, res passResult) (report.Report, error) {
	m := res.merged
	sort.SliceStable(m.kept, func(i, j int) bool {
		a, b := m.kept[i], m.kept[j]
		if a.median != b.median {
			return a.median < b.median
		}
		if a.votes != b.votes {
			return a.votes > b.votes
		}
		sa, sb := report.Score(p.byID[a.id]), report.Score(p.byID[b.id])
		if sa != sb {
			return sa > sb
		}
		return a.id < b.id
	})
	rep := base
	rep.ModelPass, rep.ModelPassReason, rep.RunsDone = report.PassDone, "", res.done
	rep.Consensus = p.consensusLabel()
	rep.Ranked, rep.Dropped = p.ranked(m), p.droppedCandidates(m)
	rep.Links, rep.Hypotheses = m.links, p.showHypotheses(m.hypotheses)
	rep.Rejected, rep.DroppedClaims = m.rejected, m.dropped
	secs, err := report.Sections(p.in.Grade, rep.Ranked)
	if err != nil {
		return base, err
	}
	for i, s := range secs {
		if r, ok := m.reports[s.Dimension]; ok {
			secs[i].Text, secs[i].Claims = p.ren.show(r.Text), r.Claims
			secs[i].Origin = report.OriginModel
		}
	}
	rep.Sections = secs
	return rep, nil
}

func (p *pass) consensusLabel() string {
	switch {
	case p.s.Runs == 1:
		return report.ConsensusSingle
	case p.s.Consensus == report.ConsensusRefute:
		return report.ConsensusRefute
	}
	return report.ConsensusMajority
}

func (p *pass) ranked(m merged) []report.Ranked {
	out := make([]report.Ranked, 0, len(m.kept))
	for i, k := range m.kept {
		r := report.FromCandidate(p.byID[k.id])
		r.Rank, r.Decision, r.Votes, r.Origin = i+1, report.DecisionKeep, k.votes,
			report.OriginModel
		r.Attack, r.Claims = p.ren.show(k.first.Attack), k.first.Claims
		out = append(out, r)
	}
	return out
}

// droppedCandidates lists every candidate not kept, in input order.
func (p *pass) droppedCandidates(m merged) []report.Dropped {
	kept := map[string]bool{}
	for _, k := range m.kept {
		kept[k.id] = true
	}
	var out []report.Dropped
	for _, c := range p.in.Candidates {
		if kept[c.ID] {
			continue
		}
		reason := fmt.Sprintf("kept by %d of %d runs", m.votes[c.ID], p.s.Runs)
		if why, ok := m.refuted[c.ID]; ok {
			reason = "refuted: " + why
		}
		out = append(out, report.Dropped{CandidateID: c.ID, Reason: reason,
			Votes: m.votes[c.ID]})
	}
	return out
}

func (p *pass) showHypotheses(hs []report.Hypothesis) []report.Hypothesis {
	for i := range hs {
		hs[i].Title, hs[i].Target = p.ren.show(hs[i].Title), p.ren.show(hs[i].Target)
		hs[i].Rationale = p.ren.show(hs[i].Rationale)
	}
	return hs
}
