package sre

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The model output contract (AI-SRE-SPEC §11). A reply is the
// submit_review tool call, or JSON content (bare, fenced or wrapped in
// prose) for providers without tool calling. It may only reorder the
// graph's open hypotheses, ask for one catalog probe with typed
// arguments when the graph is inconclusive, and narrate claims that cite
// the evidence aliases (E1, E2, ...) the turn was given. Anything else is
// a rejection with a reason code; nothing the model says is executed.

// Rejection reasons, recorded in model_rejected events.
const (
	RejectOversized       = "oversized_output"
	RejectMalformed       = "malformed_output"
	RejectEmpty           = "empty_response"
	RejectUnknownNode     = "unknown_node"
	RejectOutOfScopeNode  = "out_of_scope_node"
	RejectRanking         = "invalid_ranking"
	RejectUnknownProbe    = "unknown_probe"
	RejectProbeArgs       = "invalid_probe_args"
	RejectProbeNotAllowed = "probe_not_allowed"
	RejectRationale       = "invalid_rationale"
	RejectClaims          = "invalid_claims"
	RejectUnknownEvidence = "unknown_evidence"
	RejectStaleEvidence   = "stale_evidence"
	RejectTimeout         = "timeout"
	RejectRateLimited     = "rate_limited"
	RejectBudget          = "budget_exhausted"
	RejectCooldown        = "cooldown"
	RejectDisabled        = "llm_disabled"
	RejectProvider        = "provider_error"
	RejectInvalidRequest  = "invalid_request"
	RejectNoTime          = "no_time"
	RejectOversizedPrompt = "oversized_prompt"
	RejectProbeBudget     = "probe_budget_exhausted"
	RejectVerifier        = "verifier"
	RejectConclusion      = "invalid_conclusion"
)

// repairable reasons get the one repair turn; the others fall back at
// once (no time or budget to spend, or a retry cannot help).
var repairable = map[string]bool{RejectOversized: true, RejectMalformed: true,
	RejectEmpty: true, RejectUnknownNode: true, RejectOutOfScopeNode: true,
	RejectRanking: true, RejectUnknownProbe: true, RejectProbeArgs: true,
	RejectProbeNotAllowed: true, RejectRationale: true, RejectClaims: true,
	RejectUnknownEvidence: true, RejectStaleEvidence: true, RejectProvider: true}

// ModelRejection is why a model reply (or turn) was not used.
type ModelRejection struct {
	Reason string
	Detail string
}

func (r *ModelRejection) Error() string {
	return "model output rejected: " + r.Reason + ": " + r.Detail
}

func reject(reason, format string, args ...any) *ModelRejection {
	return &ModelRejection{Reason: reason,
		Detail: truncateRunes(RedactText(fmt.Sprintf(format, args...)), 300)}
}

// Output limits.
const (
	reviewToolName      = "submit_review"
	maxModelOutputBytes = 16 << 10
	maxWindowSeconds    = int64(probes.MaxWindow / time.Second)
)

type wireProbe struct {
	Probe     string          `json:"probe"`
	Args      json.RawMessage `json:"args"`
	Rationale string          `json:"rationale"`
}

type wireArgs struct {
	PID           *int32  `json:"pid"`
	BackendStart  *string `json:"backend_start"`
	WindowSeconds *int64  `json:"window_seconds"`
}

// modelOutput is one decoded reply, not yet checked.
type modelOutput struct {
	Ranking   []string   `json:"ranking"`
	NextProbe *wireProbe `json:"next_probe"`
	Claims    []Claim    `json:"claims"`
}

// ProposedProbe is a checked next probe: a catalog id with typed args.
type ProposedProbe struct {
	ID        probes.ID
	Args      probes.Args
	Rationale string
}

// modelReview is a checked reply. Claims cite evidence aliases; aliases
// binds each cited alias to the stored evidence id it meant.
type modelReview struct {
	Ranking   []string
	NextProbe *ProposedProbe
	Claims    []Claim
	aliases   map[string]UUID
}

// parseModelReply decodes the reply: the review tool call or JSON content.
func parseModelReply(res llm.ToolResult) (modelOutput, *ModelRejection) {
	raw, rej := replyText(res)
	if rej != nil {
		return modelOutput{}, rej
	}
	if len(raw) > maxModelOutputBytes {
		return modelOutput{}, reject(RejectOversized, "reply is %d bytes, over %d",
			len(raw), maxModelOutputBytes)
	}
	cleaned := strings.TrimSpace(llm.StripJSON(raw, llm.JSONObject))
	if cleaned == "" {
		return modelOutput{}, reject(RejectEmpty, "the reply is empty")
	}
	dec := json.NewDecoder(strings.NewReader(cleaned))
	dec.DisallowUnknownFields()
	var out modelOutput
	if err := dec.Decode(&out); err != nil {
		return modelOutput{}, reject(RejectMalformed, "not the review JSON object: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return modelOutput{}, reject(RejectMalformed, "text after the review JSON object")
	}
	return out, nil
}

func replyText(res llm.ToolResult) (string, *ModelRejection) {
	switch len(res.ToolCalls) {
	case 0:
		if strings.TrimSpace(res.Content) == "" {
			return "", reject(RejectEmpty, "the reply is empty")
		}
		return res.Content, nil
	case 1:
		if res.ToolCalls[0].Name != reviewToolName {
			return "", reject(RejectMalformed, "called %q, not %s", res.ToolCalls[0].Name,
				reviewToolName)
		}
		return string(res.ToolCalls[0].Arguments), nil
	default:
		return "", reject(RejectMalformed, "%d tool calls, want one %s call",
			len(res.ToolCalls), reviewToolName)
	}
}

// reviewScope is what one turn may rank, cite and ask for.
type reviewScope struct {
	diagnosis  causal.Diagnosis
	evidence   evidenceCatalog
	allowProbe bool
	open       []string
}

// newReviewScope binds a diagnosis and the stored evidence (aliases in
// store order) for one turn; allowProbe offers the catalog.
func newReviewScope(d causal.Diagnosis, ev []Evidence, allowProbe bool) reviewScope {
	return reviewScope{diagnosis: d, evidence: buildEvidenceCatalog(d, ev),
		allowProbe: allowProbe, open: openNodes(d)}
}

// openNodes lists the hypotheses that are not ruled out, in diagnosis
// order, once each.
func openNodes(d causal.Diagnosis) []string {
	var hs []causal.Hypothesis
	if d.Root != nil {
		hs = append(hs, *d.Root)
	}
	hs = append(append(hs, d.Contributing...), d.Alternatives...)
	seen := map[string]bool{}
	var out []string
	for _, h := range hs {
		if n := string(h.Node); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// check validates a decoded reply against the scope.
func (s reviewScope) check(out modelOutput) (modelReview, *ModelRejection) {
	if rej := s.checkRanking(out.Ranking); rej != nil {
		return modelReview{}, rej
	}
	probe, rej := s.checkProbe(out.NextProbe)
	if rej != nil {
		return modelReview{}, rej
	}
	claims, aliases, rej := s.checkClaims(out.Claims)
	if rej != nil {
		return modelReview{}, rej
	}
	return modelReview{Ranking: append([]string(nil), out.Ranking...), NextProbe: probe,
		Claims: claims, aliases: aliases}, nil
}

// checkRanking accepts exactly a permutation of the open hypotheses.
func (s reviewScope) checkRanking(ranking []string) *ModelRejection {
	open := map[string]bool{}
	for _, n := range s.open {
		open[n] = true
	}
	seen := map[string]bool{}
	for _, n := range ranking {
		if _, ok := causal.NodeByID(causal.NodeID(n)); !ok {
			return reject(RejectUnknownNode, "%q is not a node of the causal graph", n)
		}
		if !open[n] {
			return reject(RejectOutOfScopeNode, "%q is not an open hypothesis of this "+
				"diagnosis (open: %s)", n, strings.Join(s.open, ", "))
		}
		if seen[n] {
			return reject(RejectRanking, "%q is ranked twice", n)
		}
		seen[n] = true
	}
	if len(ranking) != len(s.open) {
		return reject(RejectRanking, "ranked %d of the %d open hypotheses (%s)",
			len(ranking), len(s.open), strings.Join(s.open, ", "))
	}
	return nil
}

// checkProbe accepts one catalog probe with typed args, only when the
// scope offers probes, with a one-line rationale.
func (s reviewScope) checkProbe(w *wireProbe) (*ProposedProbe, *ModelRejection) {
	if w == nil {
		return nil, nil
	}
	if !s.allowProbe {
		return nil, reject(RejectProbeNotAllowed, "next_probe is only allowed while the "+
			"graph is inconclusive")
	}
	id := probes.ID(w.Probe)
	if _, ok := probes.Catalog().Spec(id); !ok {
		return nil, reject(RejectUnknownProbe, "%q is not a catalog probe", w.Probe)
	}
	args, err := decodeProbeArgs(w.Args)
	if err == nil {
		err = probes.Catalog().CheckArgs(id, args)
	}
	if err != nil {
		return nil, reject(RejectProbeArgs, "%s: %v", id, err)
	}
	why := strings.TrimSpace(w.Rationale)
	if err := checkText("rationale", why, true, maxRationaleRunes); err != nil {
		return nil, reject(RejectRationale, "one line of 1-%d characters: %v",
			maxRationaleRunes, err)
	}
	return &ProposedProbe{ID: id, Args: args, Rationale: why}, nil
}

func decodeProbeArgs(raw json.RawMessage) (probes.Args, error) {
	var a probes.Args
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return a, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var w wireArgs
	if err := dec.Decode(&w); err != nil {
		return a, fmt.Errorf("args: %v", err)
	}
	if w.PID != nil {
		a.PID = *w.PID
	}
	if w.BackendStart != nil {
		t, err := time.Parse(time.RFC3339Nano, *w.BackendStart)
		if err != nil {
			return a, fmt.Errorf("backend_start must be an RFC 3339 timestamp")
		}
		a.BackendStart = t.UTC()
	}
	if w.WindowSeconds != nil {
		if *w.WindowSeconds <= 0 || *w.WindowSeconds > maxWindowSeconds {
			return a, fmt.Errorf("window_seconds must be in [60, %d]", maxWindowSeconds)
		}
		a.Window = time.Duration(*w.WindowSeconds) * time.Second
	}
	return a, nil
}

// checkClaims normalizes whitespace and validates the claims: in-scope,
// fresh evidence aliases and numbers grounded in the cited evidence.
func (s reviewScope) checkClaims(claims []Claim) ([]Claim, map[string]UUID, *ModelRejection) {
	if len(claims) == 0 {
		return nil, nil, nil
	}
	out := make([]Claim, 0, len(claims))
	aliases := map[string]UUID{}
	for i, c := range claims {
		text := strings.Join(strings.Fields(c.Text), " ")
		if err := checkText("claim", text, false, MaxClaimRunes); err != nil {
			return nil, nil, reject(RejectClaims, "claim %d: %v", i+1, err)
		}
		for _, alias := range c.EvidenceIDs {
			e, ok := s.evidence.byAlias(alias)
			if ok && e.stale {
				return nil, nil, reject(RejectStaleEvidence, "claim %d cites %s, whose "+
					"stored evidence no longer matches its hash", i+1, alias)
			}
			if ok {
				aliases[alias] = e.id
			}
		}
		out = append(out, Claim{Text: text, EvidenceIDs: append([]string(nil),
			c.EvidenceIDs...)})
	}
	if err := ValidateClaims(out, s.evidence.texts()); err != nil {
		if errors.Is(err, ErrUnknownEvidence) {
			return nil, nil, reject(RejectUnknownEvidence, "%v", err)
		}
		return nil, nil, reject(RejectClaims, "%v", err)
	}
	return out, aliases, nil
}
