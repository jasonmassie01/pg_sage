package sre

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Incident memory (AI-SRE-SPEC §4 R2). Similar past investigations of the
// same database (same trigger kind or family, overlapping graph nodes and
// evidence features) and their verified outcomes are offered to the model
// turn as redacted, fenced, size-bounded context, and shown in the Cases
// panel. They are never evidence: the model cannot cite them, and the
// deterministic diagnosis never reads them. The leakage guard keeps
// benchmark and replay runs honest: an investigation never sees itself,
// anything created or concluded after its own start, an outcome recorded
// after its start, or an earlier investigation of its own case, incident
// or trigger.

// Memory limits and labels.
const (
	maxMemoryExamples   = 3
	maxSimilarListed    = 5
	maxMemoryBytes      = 2048
	maxMemoryEntryBytes = 600
	memoryCandidates    = 200
	SimilarLabel        = "similar past incident (context only, not evidence)"
	MemoryLabel         = "similar past incidents offered to the model as context " +
		"(not evidence)"
)

// OutcomeVerdict is an operator's verdict on a finished investigation.
type OutcomeVerdict string

// Outcome verdicts.
const (
	OutcomeConfirmed OutcomeVerdict = "confirmed"
	OutcomeRefuted   OutcomeVerdict = "refuted"
)

// Outcome is the verified outcome of an investigation: an operator
// confirmed or refuted its conclusion, optionally naming the actual node.
type Outcome struct {
	Verdict    OutcomeVerdict `json:"verdict"`
	ActualNode string         `json:"actual_node,omitempty"`
	Actor      string         `json:"actor"`
	RecordedAt time.Time      `json:"recorded_at"`
}

// OutcomeRequest records an outcome.
type OutcomeRequest struct {
	Verdict    string
	ActualNode string
	Actor      string
}

// SimilarIncident is one past investigation offered as context.
type SimilarIncident struct {
	Label           string      `json:"label"`
	InvestigationID UUID        `json:"investigation_id"`
	CaseID          string      `json:"case_id"`
	TriggerKind     TriggerKind `json:"trigger_kind"`
	Family          string      `json:"family"`
	State           State       `json:"state"`
	Root            string      `json:"root,omitempty"`
	Open            []string    `json:"open"`
	RuledOut        []string    `json:"ruled_out"`
	ConcludedAt     time.Time   `json:"concluded_at"`
	Score           float64     `json:"score"`
	Outcome         *Outcome    `json:"outcome"`
	features        []string
}

// SimilarQuery asks for the investigations similar to Target.
type SimilarQuery struct {
	Target   Investigation
	Family   string
	Features []string
	Limit    int
}

// MemorySource is the store surface that finds similar investigations; a
// store without it offers no memory.
type MemorySource interface {
	SimilarInvestigations(ctx context.Context, scope Scope,
		q SimilarQuery) ([]SimilarIncident, error)
}

// MemoryRef records which past investigations a model turn was offered.
type MemoryRef struct {
	Label            string `json:"label"`
	InvestigationIDs []UUID `json:"investigation_ids"`
}

func (m *MemoryRef) validate() error {
	if m == nil {
		return nil
	}
	if m.Label != MemoryLabel || len(m.InvestigationIDs) == 0 ||
		len(m.InvestigationIDs) > maxMemoryExamples {
		return fmt.Errorf("%w: memory needs its label and 1-%d investigations",
			ErrInvalidRequest, maxMemoryExamples)
	}
	for _, id := range m.InvestigationIDs {
		if _, err := ParseUUID(string(id)); err != nil {
			return err
		}
	}
	return nil
}

// featureSet collects sorted, unique features.
type featureSet map[string]bool

func (f featureSet) sorted() []string {
	out := make([]string, 0, len(f))
	for k := range f {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diagnosisFeatures describes a live diagnosis exactly as recordFeatures
// describes its persisted conclusion: the root (only when conclusive, as
// conclusionOf stores it), open and ruled-out nodes, and missing probes.
func diagnosisFeatures(d causal.Diagnosis) []string {
	f := featureSet{}
	if d.Root != nil && d.Conclusive {
		f["root:"+string(d.Root.Node)], f["open:"+string(d.Root.Node)] = true, true
	}
	for _, h := range append(append([]causal.Hypothesis(nil), d.Contributing...),
		d.Alternatives...) {
		f["open:"+string(h.Node)] = true
	}
	for _, h := range d.RuledOut {
		f["ruled_out:"+string(h.Node)] = true
	}
	for _, m := range d.Missing {
		f["missing:"+string(m.ProbeID)] = true
	}
	return f.sorted()
}

// recordFeatures describes a persisted conclusion (latest revision).
func recordFeatures(hs []HypothesisRecord, s Summary) []string {
	f := featureSet{}
	for _, h := range hs {
		switch h.Status {
		case HypothesisRoot:
			f["root:"+h.Node], f["open:"+h.Node] = true, true
		case HypothesisRuledOut:
			f["ruled_out:"+h.Node] = true
		default:
			f["open:"+h.Node] = true
		}
	}
	for _, m := range s.Missing {
		f["missing:"+m.ProbeID] = true
	}
	return f.sorted()
}

// similarity is the Jaccard index of two feature sets, to 2 decimals.
func similarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	in := map[string]bool{}
	for _, x := range a {
		in[x] = true
	}
	union, both := len(in), 0
	for _, x := range b {
		if in[x] {
			both++
		} else {
			union++
		}
	}
	return math.Round(float64(both)/float64(union)*100) / 100
}

// rankSimilar scores candidates against the target's features and keeps
// the best limit with any overlap: highest score, then the most recent,
// then the id.
func rankSimilar(items []SimilarIncident, target []string, limit int) []SimilarIncident {
	out := []SimilarIncident{}
	for _, s := range items {
		s.Score = similarity(target, s.features)
		if s.Score > 0 {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Score != b.Score:
			return a.Score > b.Score
		case !a.ConcludedAt.Equal(b.ConcludedAt):
			return a.ConcludedAt.After(b.ConcludedAt)
		}
		return a.InvestigationID < b.InvestigationID
	})
	if limit < len(out) {
		out = out[:max(limit, 0)]
	}
	return out
}

// memoryBlock renders at most maxMemoryExamples incidents for the prompt,
// each redacted and bounded so the block stays within maxMemoryBytes.
func memoryBlock(items []SimilarIncident, at time.Time) string {
	var lines []string
	for i, s := range items {
		if i == maxMemoryExamples {
			break
		}
		line := fmt.Sprintf("P%d: %s investigation (family %s), %s %s before this one; "+
			"graph root %s; open: %s; ruled out: %s; outcome: %s; overlap %.2f.", i+1,
			s.TriggerKind, s.Family, s.State, age(at.Sub(s.ConcludedAt)), orNone(s.Root),
			joinOrNone(s.Open), joinOrNone(s.RuledOut), outcomeText(s.Outcome), s.Score)
		lines = append(lines, truncateBytes(RedactText(line), maxMemoryEntryBytes))
	}
	return strings.Join(lines, "\n")
}

func age(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return "<1h"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func joinOrNone(xs []string) string { return orNone(strings.Join(xs, ", ")) }

func outcomeText(o *Outcome) string {
	if o == nil {
		return "unverified"
	}
	actual := ""
	if o.ActualNode != "" {
		actual = " (actual: " + o.ActualNode + ")"
	}
	if o.Verdict == OutcomeConfirmed {
		return "operator confirmed the conclusion" + actual
	}
	return "operator refuted the conclusion" + actual
}

// truncateBytes keeps at most n bytes of s without splitting a rune.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// recall finds the similar past incidents of the investigation for the
// model turn. A failed lookup only drops the memory.
func (s *modelSession) recall(ctx context.Context, d causal.Diagnosis) {
	src, ok := s.c.store.(MemorySource)
	if !ok {
		return
	}
	items, err := src.SimilarInvestigations(ctx, s.lease.Scope, SimilarQuery{
		Target: s.inv, Family: string(d.Family), Features: diagnosisFeatures(d),
		Limit: maxMemoryExamples})
	if err != nil {
		s.c.logFn("WARN", "sre: investigation %s: reading similar past incidents "+
			"failed; the model turn goes without them: %v", s.inv.ID, err)
		return
	}
	if s.memory = memoryBlock(items, s.inv.CreatedAt); s.memory == "" {
		return
	}
	ref := &MemoryRef{Label: MemoryLabel}
	for _, it := range items[:min(len(items), maxMemoryExamples)] {
		ref.InvestigationIDs = append(ref.InvestigationIDs, it.InvestigationID)
	}
	s.memoryRef = ref
}
