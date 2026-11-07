package safetybench

import (
	"fmt"
	"strings"
)

// canonicalDesign is the design a case's Expect class is declared for (see
// testdata/readonly/README.md): the privilege-based read-only role.
const canonicalDesign = "privilege_role"

// ExpectMismatch is a case whose canonical design observed a class other
// than the one the case declares.
type ExpectMismatch struct {
	ID       string       `json:"id"`
	Expect   RefusalClass `json:"expect"`
	Observed RefusalClass `json:"observed"`
}

// Fails reports whether the mismatch fails the bench: the canonical design
// refused the statement, but by a different credited mechanism than the
// case declares, so the case no longer exercises what it says it does. An
// execution (NOT HELD) or an other_error (not measured) is listed too, but
// is already scored distinctly in the read-only table and is a measured
// result, not a harness fault.
func (m ExpectMismatch) Fails() bool { return m.Observed.Credited() }

// ExpectMismatches lists, in report order, every case whose canonical
// design attempt observed a class other than its Expect.
func (r Report) ExpectMismatches() []ExpectMismatch {
	var out []ExpectMismatch
	for _, c := range r.ReadOnly {
		for _, a := range c.Attempts {
			if a.Design != canonicalDesign || a.Observed == c.Expect {
				continue
			}
			out = append(out, ExpectMismatch{ID: c.ID, Expect: c.Expect, Observed: a.Observed})
		}
	}
	return out
}

// SelfCheckFailures returns one line per harness fault in r's self-check
// cases. Every design in r.Designs must hold every self-check with a
// credited refusal (never other_error) and intact checksums; a design with
// no attempt on a self-check, or a report with no self-checks at all, is a
// failure, and so are self-checks in a report that names no designs. An
// empty result means the harness proved itself.
func SelfCheckFailures(r Report) []string {
	var out []string
	seen := false
	for _, c := range r.ReadOnly {
		if !c.SelfCheck && !isSelfCheck(c.ID) {
			continue
		}
		seen = true
		byDesign := make(map[string]Attempt, len(c.Attempts))
		for _, a := range c.Attempts {
			byDesign[a.Design] = a
		}
		for _, d := range r.Designs {
			a, ok := byDesign[d]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("self-check %s has no attempt by %s", c.ID, d))
			case !a.Held():
				out = append(out, fmt.Sprintf(
					"self-check %s not held by %s: observed=%s intact=%v detail=%q",
					c.ID, d, a.Observed, a.ChecksumsIntact, a.Detail))
			}
		}
	}
	switch {
	case !seen:
		out = append(out, "no self-check cases ran; the embedded fixtures are missing")
	case len(r.Designs) == 0:
		out = append(out, "the report names no read-only designs; nothing was self-checked")
	}
	return out
}

func isSelfCheck(id string) bool { return strings.HasPrefix(id, "sc-") }
