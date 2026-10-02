package runbook

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// Validation checks a runbook against the catalogs before it can be
// stored or signed: catalog probes with typed args, existing graph nodes,
// known trigger kinds and action types, deterministic predicates and a
// DAG whose every path ends in a proposal. Each problem has a code a
// compiler repair turn (and an operator) can act on.

func hasCode(problems Problems, code string) bool {
	for _, p := range problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

func node(d *Definition, id string) *Node {
	for i := range d.Nodes {
		if d.Nodes[i].ID == id {
			return &d.Nodes[i]
		}
	}
	panic("no node " + id)
}

var problemCases = []struct {
	name   string
	mutate func(*Definition)
	code   string
}{
	{"empty name", func(d *Definition) { d.Name = " " }, CodeInvalid},
	{"long name", func(d *Definition) { d.Name = strings.Repeat("n", MaxNameRunes+1) },
		CodeTooLarge},
	{"control chars", func(d *Definition) { d.Name = "a\x00b" }, CodeInvalid},
	{"long description", func(d *Definition) {
		d.Description = strings.Repeat("d", MaxDescriptionRunes+1)
	}, CodeTooLarge},
	{"no trigger kinds", func(d *Definition) { d.Trigger.Kinds = nil }, CodeInvalid},
	{"unknown trigger kind", func(d *Definition) {
		d.Trigger.Kinds = []string{"disk_full"}
	}, CodeUnknownTrigger},
	{"duplicate trigger kind", func(d *Definition) {
		d.Trigger.Kinds = []string{"lock_blocking", "lock_blocking"}
	}, CodeInvalid},
	{"unknown trigger node", func(d *Definition) {
		d.Trigger.Nodes = []string{"cosmic_ray"}
	}, CodeUnknownNode},
	{"no start", func(d *Definition) { d.Start = "" }, CodeEdge},
	{"dangling start", func(d *Definition) { d.Start = "nowhere" }, CodeEdge},
	{"duplicate id", func(d *Definition) { node(d, "escalate").ID = "end_tx" },
		CodeDuplicate},
	{"bad id", func(d *Definition) {
		node(d, "read_long_tx").ID = "Read Long"
		d.Start = "Read Long"
	}, CodeInvalid},
	{"unknown type", func(d *Definition) { node(d, "escalate").Type = "sql" },
		CodeInvalid},
	{"unknown probe", func(d *Definition) { node(d, "read_long_tx").Probe = "pg_sleep" },
		CodeUnknownProbe},
	{"backend probe", func(d *Definition) {
		node(d, "read_long_tx").Probe = "backend_identity"
	}, CodeProbeArgs},
	{"window on argless probe", func(d *Definition) {
		node(d, "read_long_tx").Args = &ProbeArgs{WindowSeconds: 600}
	}, CodeProbeArgs},
	{"window too short", func(d *Definition) {
		n := node(d, "read_long_tx")
		n.Probe, n.Args = "sage_actions", &ProbeArgs{WindowSeconds: 59}
	}, CodeProbeArgs},
	{"window too long", func(d *Definition) {
		n := node(d, "read_long_tx")
		n.Probe, n.Args = "sage_actions", &ProbeArgs{WindowSeconds: 7*24*3600 + 1}
	}, CodeProbeArgs},
	{"window overflowing a duration", func(d *Definition) {
		n := node(d, "read_long_tx")
		n.Probe, n.Args = "sage_actions", &ProbeArgs{WindowSeconds: math.MaxInt64}
	}, CodeProbeArgs},
	{"negative window", func(d *Definition) {
		n := node(d, "read_long_tx")
		n.Probe, n.Args = "sage_actions", &ProbeArgs{WindowSeconds: -60}
	}, CodeProbeArgs},
	{"probe without next", func(d *Definition) { node(d, "read_long_tx").Next = "" },
		CodeEdge},
	{"probe with a predicate", func(d *Definition) {
		node(d, "read_long_tx").When = &Predicate{Op: OpProbeStatus,
			Probe: "lock_graph", In: []string{"ok"}}
	}, CodeInvalid},
	{"decision without predicate", func(d *Definition) { node(d, "old_tx").When = nil },
		CodePredicate},
	{"decision without else", func(d *Definition) { node(d, "old_tx").Else = "" },
		CodeEdge},
	{"dangling then", func(d *Definition) { node(d, "old_tx").Then = "gone" }, CodeEdge},
	{"proposal with next", func(d *Definition) { node(d, "escalate").Next = "end_tx" },
		CodeInvalid},
	{"proposal missing", func(d *Definition) { node(d, "escalate").Proposal = nil },
		CodeProposal},
	{"unknown proposal kind", func(d *Definition) {
		node(d, "escalate").Proposal.Kind = "execute_sql"
	}, CodeProposal},
	{"operator step without node", func(d *Definition) {
		node(d, "end_tx").Proposal.Node = ""
	}, CodeProposal},
	{"operator step unknown node", func(d *Definition) {
		node(d, "end_tx").Proposal.Node = "made_up"
	}, CodeUnknownNode},
	{"unknown action", func(d *Definition) {
		node(d, "end_tx").Proposal = &Proposal{Kind: ProposalAction,
			ActionType: "drop_database", Node: "idle_in_tx_holder"}
	}, CodeUnknownAction},
	{"action without node", func(d *Definition) {
		node(d, "end_tx").Proposal = &Proposal{Kind: ProposalAction,
			ActionType: "cancel_backend"}
	}, CodeProposal},
	{"escalate with node", func(d *Definition) {
		node(d, "escalate").Proposal.Node = "idle_in_tx_holder"
	}, CodeProposal},
	{"long note", func(d *Definition) {
		node(d, "escalate").Note = strings.Repeat("x", MaxNoteRunes+1)
	}, CodeTooLarge},
	{"cycle", func(d *Definition) {
		node(d, "old_tx").Else = "read_long_tx"
	}, CodeCycle},
	{"unreachable", func(d *Definition) {
		d.Nodes = append(d.Nodes, Node{ID: "orphan", Type: NodeProposal,
			Proposal: &Proposal{Kind: ProposalEscalate}})
	}, CodeUnreachable},
}

func TestValidate_ReportsEachProblemWithItsCode(t *testing.T) {
	for _, c := range problemCases {
		t.Run(c.name, func(t *testing.T) {
			d := lockRunbook()
			c.mutate(&d)
			problems := Validate(d, testVocab)
			if !hasCode(problems, c.code) {
				t.Fatalf("problems = %v, want code %s", problems, c.code)
			}
			if problems.Error() == "" || !strings.Contains(problems.Error(), c.code) {
				t.Fatalf("Error() = %q does not name %s", problems.Error(), c.code)
			}
		})
	}
}

var predicateCases = []struct {
	name string
	p    Predicate
	code string
}{
	{"unknown op", Predicate{Op: "sql", Probe: "lock_graph"}, CodePredicate},
	{"status of unknown probe", Predicate{Op: OpProbeStatus, Probe: "nope",
		In: []string{"ok"}}, CodeUnknownProbe},
	{"unknown status", Predicate{Op: OpProbeStatus, Probe: "lock_graph",
		In: []string{"healthy"}}, CodePredicate},
	{"no statuses", Predicate{Op: OpProbeStatus, Probe: "lock_graph"}, CodePredicate},
	{"row count without value", Predicate{Op: OpRowCount, Probe: "lock_graph",
		Cmp: ">="}, CodePredicate},
	{"row count negative", Predicate{Op: OpRowCount, Probe: "lock_graph", Cmp: ">=",
		Value: num(-1)}, CodePredicate},
	{"bad cmp", Predicate{Op: OpRowCount, Probe: "lock_graph", Cmp: "=~",
		Value: num(1)}, CodePredicate},
	{"unknown column", Predicate{Op: OpColumn, Probe: "lock_graph",
		Column: "blocker_password", Agg: AggMax, Cmp: ">", Value: num(1)},
		CodeUnknownColumn},
	{"bad agg", Predicate{Op: OpColumn, Probe: "lock_graph",
		Column: "blocker_xact_age_s", Agg: "avg", Cmp: ">", Value: num(1)},
		CodePredicate},
	{"NaN value", Predicate{Op: OpColumn, Probe: "lock_graph",
		Column: "blocker_xact_age_s", Agg: AggMax, Cmp: ">", Value: &nan},
		CodePredicate},
	{"text without text", Predicate{Op: OpColumnText, Probe: "lock_graph",
		Column: "blocker_state"}, CodePredicate},
	{"text too long", Predicate{Op: OpColumnText, Probe: "lock_graph",
		Column: "blocker_state", Text: strings.Repeat("t", MaxTextRunes+1)},
		CodePredicate},
	{"text unknown column", Predicate{Op: OpColumnText, Probe: "lock_graph",
		Column: "query", Text: "idle"}, CodeUnknownColumn},
	{"hypothesis unknown node", Predicate{Op: OpHypothesis, Node: "gremlins",
		In: []string{"root_cause"}}, CodeUnknownNode},
	{"hypothesis bad status", Predicate{Op: OpHypothesis, Node: "idle_in_tx_holder",
		In: []string{"likely"}}, CodePredicate},
	{"all of nothing", Predicate{Op: OpAll}, CodePredicate},
	{"not of two", Predicate{Op: OpNot, Of: []Predicate{
		{Op: OpProbeStatus, Probe: "lock_graph", In: []string{"ok"}},
		{Op: OpProbeStatus, Probe: "lock_graph", In: []string{"ok"}}}}, CodePredicate},
	{"leaf with children", Predicate{Op: OpProbeStatus, Probe: "lock_graph",
		In: []string{"ok"}, Of: []Predicate{{Op: OpAll}}}, CodePredicate},
	{"stray field", Predicate{Op: OpHypothesis, Node: "idle_in_tx_holder",
		In: []string{"root_cause"}, Column: "x"}, CodePredicate},
	{"too deep", deepPredicate(MaxPredicateDepth + 1), CodePredicate},
	{"too wide", widePredicate(MaxPredicateChildren + 1), CodePredicate},
}

func TestValidate_PredicateProblems(t *testing.T) {
	for _, c := range predicateCases {
		t.Run(c.name, func(t *testing.T) {
			d := lockRunbook()
			p := c.p
			node(&d, "old_tx").When = &p
			if problems := Validate(d, testVocab); !hasCode(problems, c.code) {
				t.Fatalf("problems = %v, want %s", problems, c.code)
			}
		})
	}
}

func deepPredicate(depth int) Predicate {
	p := Predicate{Op: OpProbeStatus, Probe: "lock_graph", In: []string{"ok"}}
	for i := 1; i < depth; i++ {
		p = Predicate{Op: OpNot, Of: []Predicate{p}}
	}
	return p
}

func widePredicate(n int) Predicate {
	p := Predicate{Op: OpAny}
	for i := 0; i < n; i++ {
		p.Of = append(p.Of, Predicate{Op: OpProbeStatus, Probe: "lock_graph",
			In: []string{"ok"}})
	}
	return p
}

func TestValidate_AcceptsEveryPredicateKind(t *testing.T) {
	d := lockRunbook()
	node(&d, "old_tx").When = &Predicate{Op: OpAll, Of: []Predicate{
		{Op: OpProbeStatus, Probe: "long_transactions", In: []string{"ok", "empty"}},
		{Op: OpRowCount, Probe: "lock_graph", Cmp: ">=", Value: num(2)},
		{Op: OpAny, Of: []Predicate{
			{Op: OpColumnText, Probe: "lock_graph", Column: "blocker_state",
				Text: "idle in transaction"},
			{Op: OpColumn, Probe: "lock_graph", Column: "blocker_xact_age_s",
				Agg: AggAll, Cmp: "!=", Value: num(0)}}},
		{Op: OpNot, Of: []Predicate{{Op: OpHypothesis, Node: "prepared_xact_holder",
			In: []string{"root_cause", "contributing"}}}},
	}}
	if problems := Validate(d, testVocab); problems != nil {
		t.Fatalf("problems = %v, want none", problems)
	}
}

func TestValidate_AcceptsActionProposalsAndWindowArgs(t *testing.T) {
	d := lockRunbook()
	n := node(&d, "read_long_tx")
	n.Probe, n.Args = "sage_actions", &ProbeArgs{WindowSeconds: 3600}
	node(&d, "old_tx").When = &Predicate{Op: OpRowCount, Probe: "sage_actions",
		Cmp: "==", Value: num(0)}
	node(&d, "end_tx").Proposal = &Proposal{Kind: ProposalAction,
		ActionType: "cancel_backend", Node: "idle_in_tx_holder"}
	if problems := Validate(d, testVocab); problems != nil {
		t.Fatalf("problems = %v, want none", problems)
	}
}

func TestValidate_SizeLimits(t *testing.T) {
	d := lockRunbook()
	for i := len(d.Nodes); i <= MaxNodes; i++ {
		d.Nodes = append(d.Nodes, Node{ID: fmt.Sprintf("extra_%d", i),
			Type: NodeProposal, Proposal: &Proposal{Kind: ProposalEscalate}})
	}
	if problems := Validate(d, testVocab); !hasCode(problems, CodeTooLarge) {
		t.Fatalf("%d nodes: problems = %v, want too_large", len(d.Nodes), problems)
	}
	chain := chainOfProbes(MaxProbeNodes + 1)
	if problems := Validate(chain, testVocab); !hasCode(problems, CodeTooLarge) {
		t.Fatalf("%d probe nodes: problems = %v, want too_large", MaxProbeNodes+1, problems)
	}
	if problems := Validate(chainOfProbes(MaxProbeNodes), testVocab); problems != nil {
		t.Fatalf("%d probe nodes: problems = %v, want none", MaxProbeNodes, problems)
	}
}

func chainOfProbes(n int) Definition {
	d := Definition{Name: "chain", Trigger: Trigger{Kinds: []string{"lock_blocking"}},
		Start: "p0"}
	for i := 0; i < n; i++ {
		next := fmt.Sprintf("p%d", i+1)
		if i == n-1 {
			next = "done"
		}
		d.Nodes = append(d.Nodes, Node{ID: fmt.Sprintf("p%d", i), Type: NodeProbe,
			Probe: "lock_graph", Next: next})
	}
	return withNode(d, Node{ID: "done", Type: NodeProposal,
		Proposal: &Proposal{Kind: ProposalEscalate}})
}

func withNode(d Definition, n Node) Definition {
	d.Nodes = append(d.Nodes, n)
	return d
}

func TestValidate_NilVocabularyKnowsNoTriggers(t *testing.T) {
	if problems := Validate(lockRunbook(), Vocab{}); !hasCode(problems, CodeUnknownTrigger) {
		t.Fatalf("empty vocabulary: problems = %v, want unknown_trigger", problems)
	}
}

func TestValidate_EmptyDefinition(t *testing.T) {
	problems := Validate(Definition{}, testVocab)
	for _, code := range []string{CodeInvalid, CodeEdge} {
		if !hasCode(problems, code) {
			t.Errorf("empty definition: problems = %v, want %s", problems, code)
		}
	}
	if Problems(nil).Error() != "" {
		t.Fatal("no problems must render as an empty string")
	}
}
