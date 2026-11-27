package plan

import (
	"fmt"
	"strings"
)

// Script renders the plan as one commented SQL script for an operator.
// Each statement carries its phase and lock level; the backfill says how
// to repeat it. pg_sage never runs it.
func (p SwapPlan) Script() string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Online column swap of %s.%s.%s: %s -> %s (pg_sage, recommend-only).\n",
		p.Schema, p.Table, p.Column, p.FromType, p.ToType)
	b.WriteString("-- Run it in one psql session, step by step; pg_sage does not run it.\n")
	if p.Degraded {
		b.WriteString("-- This build has no SQL parser: the lock levels below are the " +
			"planner's, not confirmed by the classifier.\n")
	}
	phase := SwapPhase("")
	for _, s := range p.Steps {
		if s.Phase != phase {
			phase = s.Phase
			fmt.Fprintf(&b, "\n-- == %s ==\n", phase)
		}
		fmt.Fprintf(&b, "-- step %s\n-- lock: %s%s\n", s.Name, s.LockLevel, stepFlags(s))
		if s.Note != "" {
			fmt.Fprintf(&b, "-- %s\n", s.Note)
		}
		b.WriteString(s.SQL)
		b.WriteString(";\n")
	}
	return b.String()
}

func stepFlags(s SwapStep) string {
	var flags []string
	if s.Repeat {
		flags = append(flags, "repeat until no rows")
	}
	if s.RequiresTopLevel {
		flags = append(flags, "outside a transaction block")
	}
	if s.Destructive {
		flags = append(flags, "destructive")
	}
	if len(flags) == 0 {
		return ""
	}
	return " (" + strings.Join(flags, ", ") + ")"
}
