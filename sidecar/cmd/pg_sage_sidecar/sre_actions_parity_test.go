package main

import "github.com/pg-sage/sidecar/internal/fleet"

// paritySREActions reports whether a runtime registered its Sage SRE
// action service (M5): every mode proposes actions the same way.
func paritySREActions(inst *fleet.DatabaseInstance) string {
	if inst.Investigations == nil || inst.Investigations.Actions() == nil {
		return "missing"
	}
	return "registered"
}
