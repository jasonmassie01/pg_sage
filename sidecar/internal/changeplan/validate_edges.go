package changeplan

import "sort"

// edges checks that the edges form a DAG over the plan's own steps: no
// self edge, no edge naming an ordinal the plan does not have, no
// duplicate and no cycle. An empty edge list is valid (steps then run in
// ordinal order).
func (v *validator) edges() {
	own := map[int]bool{}
	for _, s := range v.plan.Steps {
		own[s.Ordinal] = true
	}
	next := map[int][]int{}
	seen := map[Edge]bool{}
	for _, e := range v.plan.Edges {
		switch {
		case e.From == e.To:
			v.add(CodeEdgeSelf, e.From, "step %d depends on itself", e.From)
			continue
		case !own[e.From] || !own[e.To]:
			bad := e.From
			if own[e.From] {
				bad = e.To
			}
			v.add(CodeEdgeForeignStep, bad, "edge %d->%d names step %d, which is not in "+
				"this plan", e.From, e.To, bad)
			continue
		case seen[e]:
			v.add(CodeEdgeDuplicate, e.From, "edge %d->%d is listed twice", e.From, e.To)
			continue
		}
		seen[e] = true
		next[e.From] = append(next[e.From], e.To)
	}
	if at, ok := findCycle(next); ok {
		v.add(CodeEdgeCycle, at, "the edges form a cycle through step %d", at)
	}
}

// findCycle runs a depth-first search from each ordinal in ascending order
// and returns the step a back edge returns to.
func findCycle(next map[int][]int) (int, bool) {
	const (
		unseen = iota
		open
		done
	)
	color := map[int]int{}
	var visit func(n int) (int, bool)
	visit = func(n int) (int, bool) {
		color[n] = open
		for _, m := range next[n] {
			switch color[m] {
			case open:
				return m, true
			case unseen:
				if at, ok := visit(m); ok {
					return at, true
				}
			}
		}
		color[n] = done
		return 0, false
	}
	starts := make([]int, 0, len(next))
	for n := range next {
		starts = append(starts, n)
	}
	sort.Ints(starts)
	for _, n := range starts {
		if color[n] == unseen {
			if at, ok := visit(n); ok {
				return at, true
			}
		}
	}
	return 0, false
}

// checkpoints refuses a step whose revert_to_checkpoint walks back to a
// checkpoint (the latest checkpoint step before it, by ordinal) that its
// edges make run after it: that checkpoint never ran when the step fails.
// A plan whose edges cycle is refused by edges and not checked here.
func (v *validator) checkpoints() {
	next := map[int][]int{}
	for _, e := range v.plan.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	if _, cyclic := findCycle(next); cyclic {
		return
	}
	for _, s := range v.plan.Steps {
		if !revertsToCheckpoint(s) {
			continue
		}
		cp := checkpointBefore(v.plan, s.Ordinal)
		if cp != 0 && reaches(next, s.Ordinal, cp) {
			v.add(CodeCheckpointNotBefore, s.Ordinal, "step %d reverts to checkpoint step "+
				"%d, but step %d depends on step %d and so runs after it", s.Ordinal, cp, cp,
				s.Ordinal)
		}
	}
}

// revertsToCheckpoint reports whether any of s's outcome actions is
// revert_to_checkpoint.
func revertsToCheckpoint(s Step) bool {
	for _, a := range []OutcomeAction{s.OnImproved, s.OnNoChange, s.OnRegressed,
		s.OnInconclusive, s.OnUnverifiable, s.OnExpired} {
		if a == ActionRevertToCheckpoint {
			return true
		}
	}
	return false
}

// reaches reports whether an edge path leads from ordinal from to to.
func reaches(next map[int][]int, from, to int) bool {
	seen := map[int]bool{}
	stack := []int{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, m := range next[n] {
			if m == to {
				return true
			}
			if !seen[m] {
				seen[m] = true
				stack = append(stack, m)
			}
		}
	}
	return false
}
