package modelpass

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// candidatePrefix names a candidate's evidence id: cand:<candidate id>.
const candidatePrefix = "cand:"

// candidateView is what the model reads of a candidate: its numbers and
// names, never its SQL or the generator's prose.
type candidateView struct {
	ID          string               `json:"candidate_id"`
	Generator   string               `json:"generator"`
	Dimension   string               `json:"dimension"`
	Kind        string               `json:"kind"`
	Target      string               `json:"target"`
	ActionType  string               `json:"action_type"`
	EvidenceIDs []string             `json:"evidence_ids"`
	Prediction  candidate.Prediction `json:"prediction"`
	Reversible  bool                 `json:"reversible"`
	Approval    string               `json:"approval"`
}

// buildSeed is a review's citable evidence: every candidate (E1..Ek, in
// input order), then every fact of the model tier by id. It also returns
// each candidate's view by id. Seeds are built once per review and only
// read by its runs.
func buildSeed(in Input) ([]agentloop.Evidence, map[string]string, error) {
	if in.Pack == nil {
		return nil, nil, fmt.Errorf("no evidence pack")
	}
	ren := newRenamer(in.Pack.Pseudonyms)
	seed := make([]agentloop.Evidence, 0, len(in.Candidates)+len(in.Pack.IDs()))
	views := make(map[string]string, len(in.Candidates))
	for _, c := range in.Candidates {
		ev, err := candidateEvidence(c, ren)
		if err != nil {
			return nil, nil, err
		}
		seed = append(seed, ev)
		views[c.ID] = ev.Text
	}
	for _, id := range in.Pack.IDs() {
		f, err := workpack.Resolve(in.Pack, id)
		if err != nil {
			return nil, nil, err
		}
		values := append([]string{id}, numbersOf(f.Value)...)
		seed = append(seed, agentloop.Evidence{ID: id, Digest: in.Pack.ContentSHA256,
			Label: f.Section + ", " + f.Unit, Text: id + " " + string(f.Value),
			Values: values})
	}
	return seed, views, nil
}

func candidateEvidence(c candidate.Candidate, ren renamer) (agentloop.Evidence, error) {
	v := candidateView{ID: c.ID, Generator: c.Generator, Dimension: c.Dimension, Kind: c.Kind,
		Target: ren.hide(c.Target), ActionType: c.ActionType, Prediction: c.Prediction,
		Reversible: c.Reversible, Approval: c.Approval}
	for _, id := range c.EvidenceIDs {
		v.EvidenceIDs = append(v.EvidenceIDs, ren.hide(id))
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return agentloop.Evidence{}, fmt.Errorf("encode candidate %s: %w", c.ID, err)
	}
	values := append([]string{c.ID, v.Target}, numbersOf(raw)...)
	return agentloop.Evidence{ID: candidatePrefix + c.ID, Digest: c.ID,
		Label: "candidate " + c.Kind + " on " + v.Target, Text: string(raw),
		Values: values}, nil
}

// numbersOf is every number in a JSON value, as its exact token.
func numbersOf(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case json.Number:
			out = append(out, t.String())
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case float64:
			out = append(out, strconv.FormatFloat(t, 'f', -1, 64))
		}
	}
	walk(v)
	sort.Strings(out)
	return out
}

// renamer swaps names for a hashed model tier's pseudonyms (hide) and
// back (show). A tier without pseudonyms changes nothing.
type renamer struct{ toPseudo, toName *strings.Replacer }

func newRenamer(pseudonyms map[string]string) renamer {
	if len(pseudonyms) == 0 {
		return renamer{}
	}
	pairs := make([][2]string, 0, len(pseudonyms))
	for p, name := range pseudonyms {
		pairs = append(pairs, [2]string{p, name})
	}
	// Longest names first, so a name inside another is not swapped early.
	sort.Slice(pairs, func(i, j int) bool {
		if len(pairs[i][1]) != len(pairs[j][1]) {
			return len(pairs[i][1]) > len(pairs[j][1])
		}
		return pairs[i][1] < pairs[j][1]
	})
	hide, show := make([]string, 0, 2*len(pairs)), make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		hide = append(hide, p[1], p[0])
		show = append(show, p[0], p[1])
	}
	return renamer{toPseudo: strings.NewReplacer(hide...), toName: strings.NewReplacer(show...)}
}

func (r renamer) hide(s string) string {
	if r.toPseudo == nil {
		return s
	}
	return r.toPseudo.Replace(s)
}

func (r renamer) show(s string) string {
	if r.toName == nil {
		return s
	}
	return r.toName.Replace(s)
}

// tools are the run's read-only tools over the in-memory pack.
func tools(pack *workpack.Pack, views map[string]string) []agentloop.Tool {
	section := agentloop.Tool{Name: "read_pack_section",
		Description: "Read one section of the evidence pack (for example workload, " +
			"indexes, schema) as JSON. Not citable: cite the seeded evidence.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"name":` +
			`{"type":"string"}},"required":["name"]}`),
		Run: func(_ context.Context, args json.RawMessage) (agentloop.Output, error) {
			var a struct{ Name string }
			if json.Unmarshal(args, &a) != nil {
				return agentloop.Output{}, agentloop.ErrInvalidArgs
			}
			name := strings.TrimSuffix(a.Name, ".json") + ".json"
			data := pack.File(name)
			if data == nil || strings.Contains(name, "/") {
				return agentloop.Output{}, fmt.Errorf("%w: no section %q",
					agentloop.ErrInvalidArgs, a.Name)
			}
			return agentloop.Output{Status: "ok", Text: string(data)}, nil
		}}
	cand := agentloop.Tool{Name: "read_candidate",
		Description: "Read one candidate by its candidate_id.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"id":` +
			`{"type":"string"}},"required":["id"]}`),
		Run: func(_ context.Context, args json.RawMessage) (agentloop.Output, error) {
			var a struct{ ID string }
			if json.Unmarshal(args, &a) != nil || views[a.ID] == "" {
				return agentloop.Output{}, fmt.Errorf("%w: unknown candidate",
					agentloop.ErrInvalidArgs)
			}
			return agentloop.Output{Status: "ok", Text: views[a.ID]}, nil
		}}
	return []agentloop.Tool{section, cand}
}
