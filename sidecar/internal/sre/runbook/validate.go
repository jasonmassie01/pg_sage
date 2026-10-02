package runbook

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Problem codes. A compiler repair turn and an operator see them.
const (
	CodeInvalid        = "invalid_definition"
	CodeTooLarge       = "too_large"
	CodeUnknownTrigger = "unknown_trigger"
	CodeUnknownNode    = "unknown_graph_node"
	CodeUnknownProbe   = "unknown_probe"
	CodeProbeArgs      = "invalid_probe_args"
	CodeUnknownColumn  = "unknown_column"
	CodeUnknownAction  = "unknown_action"
	CodeProposal       = "invalid_proposal"
	CodePredicate      = "invalid_predicate"
	CodeEdge           = "dangling_edge"
	CodeCycle          = "cycle"
	CodeUnreachable    = "unreachable_node"
	CodeDuplicate      = "duplicate_id"
)

// Problem is one reason a definition is not usable.
type Problem struct {
	Code   string `json:"code"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// Problems is every problem of a definition; nil means valid.
type Problems []Problem

func (p Problems) Error() string {
	parts := make([]string, 0, len(p))
	for _, x := range p {
		parts = append(parts, x.Code+" at "+x.Path+": "+x.Detail)
	}
	return strings.Join(parts, "; ")
}

func (p *Problems) add(code, path, format string, args ...any) {
	*p = append(*p, Problem{Code: code, Path: path, Detail: fmt.Sprintf(format, args...)})
}

// Vocab is what a definition may reference beyond the fixed catalogs: the
// investigator's trigger kinds (other milestones add families).
type Vocab struct {
	TriggerKinds []string
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

func isControl(r rune) bool { return unicode.IsControl(r) }

// isControlBesidesLayout allows newlines and tabs (multi-line text).
func isControlBesidesLayout(r rune) bool { return r != '\n' && r != '\t' && isControl(r) }

// Validate checks a definition against the probe catalog, the causal
// graph, the runbook action types and v's trigger kinds, and checks the
// DAG: unique ids, edges that resolve, no cycle, every node reachable from
// the start and every path ending in a proposal.
func Validate(d Definition, v Vocab) Problems {
	var out Problems
	checkHeader(d, v, &out)
	nodes := checkNodes(d.Nodes, &out)
	checkGraph(d, nodes, &out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func checkText(name, value string, max int, required bool, out *Problems) {
	bad := isControl
	if name == "description" {
		bad = isControlBesidesLayout
	}
	switch {
	case strings.TrimSpace(value) == "" && required:
		out.add(CodeInvalid, name, "%s is required", name)
	case utf8.RuneCountInString(value) > max:
		out.add(CodeTooLarge, name, "%s is longer than %d characters", name, max)
	case strings.IndexFunc(value, bad) >= 0:
		out.add(CodeInvalid, name, "%s contains control characters", name)
	}
}

func checkHeader(d Definition, v Vocab, out *Problems) {
	checkText("name", d.Name, MaxNameRunes, true, out)
	checkText("description", d.Description, MaxDescriptionRunes, false, out)
	kinds := d.Trigger.Kinds
	switch {
	case len(kinds) == 0:
		out.add(CodeInvalid, "trigger.kinds", "name at least one trigger kind")
	case len(kinds) > MaxTriggerKinds:
		out.add(CodeTooLarge, "trigger.kinds", "at most %d trigger kinds", MaxTriggerKinds)
	}
	seen := map[string]bool{}
	for i, k := range kinds {
		if seen[k] {
			out.add(CodeInvalid, fmt.Sprintf("trigger.kinds[%d]", i), "%q twice", k)
		}
		seen[k] = true
		if !containsString(v.TriggerKinds, k) {
			out.add(CodeUnknownTrigger, fmt.Sprintf("trigger.kinds[%d]", i),
				"%q is not a trigger kind (known: %s)", k, strings.Join(v.TriggerKinds, ", "))
		}
	}
	if len(d.Trigger.Nodes) > MaxTriggerNodes {
		out.add(CodeTooLarge, "trigger.nodes", "at most %d nodes", MaxTriggerNodes)
	}
	for i, n := range d.Trigger.Nodes {
		if !graphNode(n) {
			out.add(CodeUnknownNode, fmt.Sprintf("trigger.nodes[%d]", i),
				"%q is not a causal graph node", n)
		}
	}
}

// checkNodes validates each node and returns the nodes by id.
func checkNodes(nodes []Node, out *Problems) map[string]Node {
	byID := map[string]Node{}
	probeNodes := 0
	switch {
	case len(nodes) == 0:
		out.add(CodeInvalid, "nodes", "a runbook needs nodes")
	case len(nodes) > MaxNodes:
		out.add(CodeTooLarge, "nodes", "%d nodes, at most %d", len(nodes), MaxNodes)
	}
	for i, n := range nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		if !idPattern.MatchString(n.ID) {
			out.add(CodeInvalid, path+".id", "id %q must match %s", n.ID, idPattern)
		}
		if _, dup := byID[n.ID]; dup {
			out.add(CodeDuplicate, path+".id", "id %q is used twice", n.ID)
		}
		byID[n.ID] = n
		checkText(path+".note", n.Note, MaxNoteRunes, false, out)
		if n.Type == NodeProbe {
			probeNodes++
		}
		checkNode(n, path, out)
	}
	if probeNodes > MaxProbeNodes {
		out.add(CodeTooLarge, "nodes", "%d probe steps, at most %d", probeNodes,
			MaxProbeNodes)
	}
	return byID
}

// nodeFields lists the fields a node of each type may set.
var nodeFields = map[NodeType]map[string]bool{
	NodeProbe:    {"probe": true, "args": true, "next": true},
	NodeDecision: {"when": true, "then": true, "else": true},
	NodeProposal: {"proposal": true},
}

func (n Node) setFields() []string {
	all := []struct {
		name string
		set  bool
	}{{"probe", n.Probe != ""}, {"args", n.Args != nil}, {"next", n.Next != ""},
		{"when", n.When != nil}, {"then", n.Then != ""}, {"else", n.Else != ""},
		{"proposal", n.Proposal != nil}}
	var out []string
	for _, f := range all {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

func checkNode(n Node, path string, out *Problems) {
	allowed, ok := nodeFields[n.Type]
	if !ok {
		out.add(CodeInvalid, path+".type", "unknown node type %q", n.Type)
		return
	}
	for _, f := range n.setFields() {
		if !allowed[f] {
			out.add(CodeInvalid, path+"."+f, "a %s node does not take %s", n.Type, f)
		}
	}
	switch n.Type {
	case NodeProbe:
		checkProbeStep(n, path, out)
	case NodeDecision:
		if n.When == nil {
			out.add(CodePredicate, path+".when", "a decision needs a predicate")
		} else {
			validatePredicate(*n.When, path+".when", 1, out)
		}
	case NodeProposal:
		checkProposal(n.Proposal, path+".proposal", out)
	}
}

func checkProbeStep(n Node, path string, out *Problems) {
	spec, ok := probes.Catalog().Spec(probes.ID(n.Probe))
	if !ok {
		out.add(CodeUnknownProbe, path+".probe", "%q is not a catalog probe", n.Probe)
		return
	}
	var args probes.Args
	if n.Args != nil {
		if spec.Args != probes.ArgsWindow {
			out.add(CodeProbeArgs, path+".args", "%s takes no arguments", n.Probe)
			return
		}
		ws := n.Args.WindowSeconds
		if ws < 0 || ws > int64(probes.MaxWindow/time.Second) {
			out.add(CodeProbeArgs, path+".args.window_seconds",
				"window_seconds must be in [60, %d]", int64(probes.MaxWindow/time.Second))
			return
		}
		args.Window = secondsDuration(ws)
	}
	if spec.Args == probes.ArgsBackend {
		out.add(CodeProbeArgs, path+".probe", "%s needs a backend identity, which only "+
			"evidence can supply; runbook steps cannot use it", n.Probe)
		return
	}
	if err := probes.Catalog().CheckArgs(spec.ID, args); err != nil {
		out.add(CodeProbeArgs, path+".args", "%v", err)
	}
}

// checkGraph checks the edges, acyclicity and reachability.
func checkGraph(d Definition, byID map[string]Node, out *Problems) {
	if _, ok := byID[d.Start]; !ok || d.Start == "" {
		out.add(CodeEdge, "start", "start %q is not a node", d.Start)
		return
	}
	for i, n := range d.Nodes {
		for _, e := range edgesOf(n) {
			if _, ok := byID[e.to]; !ok {
				out.add(CodeEdge, fmt.Sprintf("nodes[%d].%s", i, e.name),
					"%q is not a node", e.to)
			}
		}
	}
	reached := map[string]bool{}
	if cyclic := walk(d.Start, byID, map[string]bool{}, reached); cyclic != "" {
		out.add(CodeCycle, "nodes", "a path returns to %q", cyclic)
	}
	for i, n := range d.Nodes {
		if !reached[n.ID] {
			out.add(CodeUnreachable, fmt.Sprintf("nodes[%d]", i),
				"%q cannot be reached from the start", n.ID)
		}
	}
}

type edge struct{ name, to string }

// edgesOf lists a node's outgoing edges; missing ones count as dangling.
func edgesOf(n Node) []edge {
	switch n.Type {
	case NodeProbe:
		return []edge{{"next", n.Next}}
	case NodeDecision:
		return []edge{{"then", n.Then}, {"else", n.Else}}
	}
	return nil
}

// walk is a depth-first search from id. It marks reached nodes and
// returns the id a path returns to, or "".
func walk(id string, byID map[string]Node, onPath, reached map[string]bool) string {
	if onPath[id] {
		return id
	}
	n, ok := byID[id]
	if !ok || reached[id] {
		return ""
	}
	reached[id] = true
	onPath[id] = true
	defer delete(onPath, id)
	for _, e := range edgesOf(n) {
		if c := walk(e.to, byID, onPath, reached); c != "" {
			return c
		}
	}
	return ""
}
