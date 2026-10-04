package sre

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Contested investigations become replay cases (roadmap 2.4). When an
// operator refutes an investigation's conclusion (or confirms it with
// another actual root), pg_sage exports it as a PGIncidentBench replay
// case (schema pg_sage.sre.replay_case.v1, sidecar/sre-bench/replay):
// the stored probe results, frozen at detection time, redacted
// (replay_redact.go), with the operator's answer as gold. The export
// re-runs the deterministic diagnosis on the redacted evidence and says
// whether the graph's root survived redaction. Promoting a case into the
// corpus is a reviewed, manual step (sidecar/sre-bench/README.md).

// Errors.
var (
	// ErrNotContested: no operator outcome contests the conclusion.
	ErrNotContested = errors.New("investigation is not contested")
	// ErrNotExportable: the investigation cannot become a valid case.
	ErrNotExportable = errors.New("investigation cannot become a replay case")
)

// Replay case limits, as the corpus validates them.
const (
	ReplayCaseSchema   = "pg_sage.sre.replay_case.v1"
	replayLookahead    = 120 * time.Second
	replayLookback     = 24 * time.Hour
	replayPromoteNotes = "Review the case, set provenance to \"redacted incident <ref>, " +
		"used with permission\", save it as sidecar/sre-bench/replay/cases/<family>/<id>" +
		".json, add its line to replay/split.lock and run the corpus tests " +
		"(sidecar/sre-bench/README.md, \"Contested production investigations\")."
)

// ReplayExportOptions: KeepIdentifiers is the operator's opt-in to
// keeping identifiers; salt fixes the hash key (tests; nil: random).
type ReplayExportOptions struct {
	KeepIdentifiers bool
	salt            []byte
}

// ReplayGold is a case's expected diagnosis.
type ReplayGold struct {
	Root         string   `json:"root,omitempty"`
	Contributing []string `json:"contributing,omitempty"`
	Lookalike    string   `json:"lookalike,omitempty"`
	Rationale    string   `json:"rationale"`
}

// ReplayObservation is one recorded probe result.
type ReplayObservation struct {
	Probe     probes.ID     `json:"probe"`
	Status    probes.Status `json:"status"`
	OffsetMS  int64         `json:"offset_ms"`
	Reason    string        `json:"reason,omitempty"`
	Error     string        `json:"error,omitempty"`
	Rows      []probes.Row  `json:"rows,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// ReplayCaseDoc is a replay case document.
type ReplayCaseDoc struct {
	Schema       string              `json:"schema"`
	ID           string              `json:"id"`
	Family       string              `json:"family"`
	Class        string              `json:"class"`
	Description  string              `json:"description"`
	Provenance   string              `json:"provenance"`
	DetectedAt   time.Time           `json:"detected_at"`
	Subject      string              `json:"subject,omitempty"`
	Tags         []string            `json:"tags,omitempty"`
	Gold         ReplayGold          `json:"gold"`
	Observations []ReplayObservation `json:"observations"`
}

// ContestInfo is the operator's contest of the conclusion.
type ContestInfo struct {
	Verdict    string    `json:"verdict"`
	ActualNode string    `json:"actual_node,omitempty"`
	GraphRoot  string    `json:"graph_root,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ReplayCaseExport is the case with what the operator needs to judge it.
type ReplayCaseExport struct {
	Case               ReplayCaseDoc `json:"case"`
	Contest            ContestInfo   `json:"contest"`
	IdentifiersKept    bool          `json:"identifiers_kept"`
	GraphRootPreserved bool          `json:"graph_root_preserved"`
	Notes              []string      `json:"notes"`
	Promote            string        `json:"promote"`
}

// ExportReplayCase exports one of this database's contested
// investigations as a redacted replay case.
func (s *Service) ExportReplayCase(ctx context.Context, id UUID,
	opts ReplayExportOptions) (ReplayCaseExport, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return ReplayCaseExport{}, err
	}
	return ExportReplayCaseFromStore(ctx, s.store, scope, id, opts)
}

// ExportReplayCaseFromStore exports an investigation read from st (the
// CLI, which finds the scope with LookupScope).
func ExportReplayCaseFromStore(ctx context.Context, st *PostgresStore, scope Scope, id UUID,
	opts ReplayExportOptions) (ReplayCaseExport, error) {
	inv, err := st.Get(ctx, scope, id)
	if err != nil {
		return ReplayCaseExport{}, err
	}
	ev, err := st.Evidence(ctx, scope, id)
	if err != nil {
		return ReplayCaseExport{}, err
	}
	out, err := st.LatestOutcome(ctx, scope, id)
	if err != nil {
		return ReplayCaseExport{}, err
	}
	return BuildReplayCase(inv, ev, out, opts)
}

// graphRootOf is the causal graph's own root (before any adopted model
// root).
func graphRootOf(s Summary) string {
	if s.ModelContest != nil && s.ModelContest.Authority == ContestAdopted {
		return s.ModelContest.GraphRoot
	}
	return s.Root
}

// BuildReplayCase builds the redacted case of a contested investigation.
func BuildReplayCase(inv Investigation, ev []Evidence, out *Outcome,
	opts ReplayExportOptions) (ReplayCaseExport, error) {
	if out == nil {
		return ReplayCaseExport{}, fmt.Errorf("%w: no operator outcome", ErrNotContested)
	}
	if inv.State != StateConcluded && inv.State != StateInconclusive {
		return ReplayCaseExport{}, fmt.Errorf("%w: investigation is %s, not finished",
			ErrNotExportable, inv.State)
	}
	family, graphRoot := familyOf(inv), graphRootOf(inv.Summary)
	gold, class, err := goldOf(family, graphRoot, out)
	if err != nil {
		return ReplayCaseExport{}, err
	}
	key := opts.salt
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return ReplayCaseExport{}, fmt.Errorf("replay export key: %w", err)
		}
	}
	r := redactor{keep: opts.KeepIdentifiers, key: key}
	obs, err := r.observations(inv.CreatedAt, ev)
	if err != nil {
		return ReplayCaseExport{}, err
	}
	id := "contested-" + strings.ReplaceAll(family, "_", "-") + "-" +
		r.token(string(inv.ID))[3:11]
	doc := ReplayCaseDoc{Schema: ReplayCaseSchema, ID: id, Family: family, Class: class,
		DetectedAt:  inv.CreatedAt.UTC().Truncate(time.Millisecond),
		Subject:     r.subject(family, inv.Subject),
		Tags:        []string{"contested", "post_r1"},
		Gold:        gold,
		Description: describeContest(graphRoot, out),
		Provenance: fmt.Sprintf("redacted production incident, contested on %s and "+
			"exported by pg_sage; promote only with the data owner's permission",
			out.RecordedAt.UTC().Format("2006-01-02")),
		Observations: obs}
	exp := ReplayCaseExport{Case: doc, IdentifiersKept: opts.KeepIdentifiers,
		Contest: ContestInfo{Verdict: string(out.Verdict), ActualNode: out.ActualNode,
			GraphRoot: graphRoot, RecordedAt: out.RecordedAt},
		Promote: replayPromoteNotes, Notes: []string{}}
	exp.GraphRootPreserved, exp.Notes = preserved(inv, doc, graphRoot)
	return exp, nil
}

func familyOf(inv Investigation) string {
	if inv.Summary.Family != "" {
		return inv.Summary.Family
	}
	return string(inv.TriggerKind)
}

// goldOf is the case's gold and class from the operator's outcome.
func goldOf(family, graphRoot string, out *Outcome) (ReplayGold, string, error) {
	actual := strings.TrimSpace(out.ActualNode)
	contested := out.Verdict == OutcomeRefuted ||
		out.Verdict == OutcomeConfirmed && actual != "" && actual != graphRoot
	if !contested {
		return ReplayGold{}, "", fmt.Errorf("%w: the operator confirmed the conclusion",
			ErrNotContested)
	}
	rationale := fmt.Sprintf("operator verdict %s on %s", out.Verdict,
		out.RecordedAt.UTC().Format("2006-01-02"))
	if actual != "" {
		n, ok := causal.NodeByID(causal.NodeID(actual))
		if !ok || string(n.Family) != family {
			return ReplayGold{}, "", fmt.Errorf("%w: the actual root %q is not a %s node; a "+
				"case's gold is a node of its own family", ErrNotExportable, actual, family)
		}
		return ReplayGold{Root: actual, Rationale: rationale + "; actual root " + actual},
			"positive", nil
	}
	if graphRoot == "" {
		return ReplayGold{}, "", fmt.Errorf("%w: an inconclusive investigation refuted "+
			"without an actual root has no gold", ErrNotExportable)
	}
	return ReplayGold{Lookalike: graphRoot, Rationale: rationale + "; the evidence " +
		"imitates " + graphRoot + " but the operator refuted it"}, "confounded", nil
}

func describeContest(graphRoot string, out *Outcome) string {
	concluded := "left it inconclusive"
	if graphRoot != "" {
		concluded = "concluded " + graphRoot
	}
	actual := ""
	if out.ActualNode != "" {
		actual = " (actual root: " + out.ActualNode + ")"
	}
	return fmt.Sprintf("Contested production investigation: the causal graph %s; the "+
		"operator's verdict was %s%s.", concluded, out.Verdict, actual)
}

// observations converts the evidence, in order, to redacted
// observations offset from detection, inside the replay window.
func (r redactor) observations(detected time.Time, ev []Evidence) ([]ReplayObservation,
	error) {
	if len(ev) == 0 {
		return nil, fmt.Errorf("%w: its evidence was purged", ErrNotExportable)
	}
	out := make([]ReplayObservation, 0, len(ev))
	for _, e := range ev {
		o, err := r.observation(detected, e)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (r redactor) observation(detected time.Time, e Evidence) (ReplayObservation, error) {
	var res probes.Result
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return ReplayObservation{}, fmt.Errorf("%w: evidence %s: %v", ErrNotExportable,
			e.ID, err)
	}
	at := e.ObservedAt
	for _, t := range []time.Time{res.ObservedAt, e.CollectedAt} {
		if at.IsZero() {
			at = t
		}
	}
	offset := at.Sub(detected)
	if offset > replayLookahead || -offset > replayLookback {
		return ReplayObservation{}, fmt.Errorf("%w: evidence %s was observed %s after "+
			"detection, outside the replay window (-%s..%s)", ErrNotExportable, e.ID,
			offset.Round(time.Second), replayLookback, replayLookahead)
	}
	o := ReplayObservation{Probe: res.ProbeID, Status: res.Status,
		OffsetMS: offset.Milliseconds(), Reason: r.code(res.Reason),
		Error: r.text(res.Error), Truncated: res.Truncated}
	limit := detected.Add(replayLookahead)
	for _, row := range res.Rows {
		if err := rowInWindow(row, limit); err != nil {
			return ReplayObservation{}, fmt.Errorf("%w: evidence %s: %v", ErrNotExportable,
				e.ID, err)
		}
		o.Rows = append(o.Rows, probes.Row(r.row(row)))
	}
	return o, nil
}

// rowInWindow refuses a row timestamp after the replay window.
func rowInWindow(row probes.Row, limit time.Time) error {
	for k, v := range row {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(limit) {
			return fmt.Errorf("column %s is after the replay window", k)
		}
	}
	return nil
}

// preserved re-runs the deterministic diagnosis on the redacted evidence
// and reports whether the graph's root (or inconclusive) survived.
func preserved(inv Investigation, doc ReplayCaseDoc, graphRoot string) (bool, []string) {
	ev := make([]Evidence, 0, len(doc.Observations))
	for _, o := range doc.Observations {
		res := probes.Result{ProbeID: o.Probe, Status: o.Status, Reason: o.Reason,
			Error: o.Error, Rows: o.Rows, Truncated: o.Truncated,
			ObservedAt: doc.DetectedAt.Add(time.Duration(o.OffsetMS) * time.Millisecond)}
		payload, err := canonicalPayload(res)
		if err != nil {
			return false, []string{"the redacted evidence could not be re-encoded: " +
				err.Error()}
		}
		ev = append(ev, Evidence{ID: NewUUID(), ProbeID: string(o.Probe), Payload: payload,
			ObservedAt: res.ObservedAt})
	}
	obs, err := observations(ev)
	if err != nil {
		return false, []string{"the redacted evidence could not be read back: " + err.Error()}
	}
	replayed := inv
	replayed.Subject = doc.Subject
	d := diagnose(replayed, obs)
	root := ""
	if d.Conclusive && d.Root != nil {
		root = string(d.Root.Node)
	}
	if root == graphRoot {
		return true, []string{}
	}
	return false, []string{fmt.Sprintf("redaction changed the causal graph's diagnosis "+
		"(%q instead of %q): review the case before promoting it, or export it with "+
		"identifiers kept", root, graphRoot)}
}
