// Package runbook is the Sage SRE typed runbook (AI-SRE-SPEC §7.1): a
// versioned DAG "trigger signature -> probe steps -> decision nodes ->
// proposal". Steps may only name catalog probes with typed arguments;
// decisions are deterministic, three-valued predicates over probe results
// and the causal graph's hypotheses; a runbook ends in a proposal that is
// never executed by the runbook. The package is pure: storage, signing and
// running live in package sre.
package runbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Definition limits.
const (
	MaxNodes             = 32
	MaxProbeNodes        = 8
	MaxPredicateDepth    = 4
	MaxPredicateChildren = 8
	MaxTriggerKinds      = 8
	MaxTriggerNodes      = 16
	MaxNameRunes         = 120
	MaxDescriptionRunes  = 1000
	MaxNoteRunes         = 200
	MaxTextRunes         = 64
	MaxDefinitionBytes   = 32 << 10
)

// ErrInvalid reports a definition (or compiler input) that cannot be used.
var ErrInvalid = errors.New("invalid runbook")

// NodeType is the kind of a DAG node.
type NodeType string

// Node types.
const (
	NodeProbe    NodeType = "probe"
	NodeDecision NodeType = "decision"
	NodeProposal NodeType = "proposal"
)

// Trigger is the signature an investigation must match: one of its trigger
// kinds and, when Nodes is set, at least one of these graph nodes open
// (not ruled out) in the investigation's diagnosis.
type Trigger struct {
	Kinds []string `json:"kinds"`
	Nodes []string `json:"nodes,omitempty"`
}

// ProbeArgs are a probe step's typed arguments. Only window probes take
// one; backend probes cannot be runbook steps (a runbook is static, and a
// backend identity only comes from evidence).
type ProbeArgs struct {
	WindowSeconds int64 `json:"window_seconds,omitempty"`
}

// Node is one DAG node. A probe node has Probe, optional Args and Next; a
// decision node has When, Then and Else; a proposal node has Proposal.
type Node struct {
	ID       string     `json:"id"`
	Type     NodeType   `json:"type"`
	Note     string     `json:"note,omitempty"`
	Probe    string     `json:"probe,omitempty"`
	Args     *ProbeArgs `json:"args,omitempty"`
	Next     string     `json:"next,omitempty"`
	When     *Predicate `json:"when,omitempty"`
	Then     string     `json:"then,omitempty"`
	Else     string     `json:"else,omitempty"`
	Proposal *Proposal  `json:"proposal,omitempty"`
}

// Definition is one runbook version's content. Its canonical JSON is what
// a signature binds.
type Definition struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Trigger     Trigger `json:"trigger"`
	Start       string  `json:"start"`
	Nodes       []Node  `json:"nodes"`
}

// Decode reads a definition strictly: one JSON object, no unknown fields,
// at most MaxDefinitionBytes. Empty probe args are normalized away so they
// hash like no args. It does not validate against the catalogs.
func Decode(raw []byte) (Definition, error) {
	var d Definition
	if len(raw) > MaxDefinitionBytes {
		return d, fmt.Errorf("%w: definition is %d bytes, over %d", ErrInvalid,
			len(raw), MaxDefinitionBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Definition{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Definition{}, fmt.Errorf("%w: data after the definition object", ErrInvalid)
	}
	return d.normalized(), nil
}

func (d Definition) normalized() Definition {
	nodes := make([]Node, len(d.Nodes))
	for i, n := range d.Nodes {
		if n.Args != nil && *n.Args == (ProbeArgs{}) {
			n.Args = nil
		}
		nodes[i] = n
	}
	if d.Nodes != nil {
		d.Nodes = nodes
	}
	return d
}

// Canonical is the definition's canonical JSON: fixed field order, no
// insignificant whitespace, empty args removed.
func (d Definition) Canonical() ([]byte, error) {
	raw, err := json.Marshal(d.normalized())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return raw, nil
}

// Hash is the lower-case hex SHA-256 of the canonical JSON.
func (d Definition) Hash() (string, error) {
	raw, err := d.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Node returns the node with id.
func (d Definition) Node(id string) (Node, bool) {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Match reports whether an investigation of trigger kind with these open
// graph nodes matches the trigger signature, and how specific the match
// is (the number of the signature's nodes that are open; 0 for a
// kind-only signature).
func (d Definition) Match(kind string, open []string) (bool, int) {
	if kind == "" || !containsString(d.Trigger.Kinds, kind) {
		return false, 0
	}
	if len(d.Trigger.Nodes) == 0 {
		return true, 0
	}
	n := 0
	for _, node := range d.Trigger.Nodes {
		if containsString(open, node) {
			n++
		}
	}
	return n > 0, n
}

func containsString(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
