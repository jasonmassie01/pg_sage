package snapstore

import (
	"bytes"
	"encoding/json"
)

// globalIncrements picks, per integer field, the increment that most
// paired elements moved by, when more elements moved by it than stayed
// unchanged. Every table's xid_age advances by the same amount each cycle,
// so a tables delta needs one entry for it, not one per table. Elements
// that moved differently carry the difference in i.
//
// A field is never given a global increment when some base element holds
// it as a number in exponent form: jsonb prints such a number as plain
// digits, so the decoder would treat it as an integer and the encoder
// would not.
func globalIncrements(base, cur *catalog) map[string]json.RawMessage {
	counts := map[string]map[string]int{}
	exponent := map[string]bool{}
	for bi, item := range base.items {
		ci, ok := cur.pos[base.keys[bi]]
		if !ok {
			continue
		}
		for f, b := range item {
			if exponentNumber(b) {
				exponent[f] = true
				continue
			}
			inc, ok := increment(b, cur.items[ci][f])
			if !ok {
				continue
			}
			if counts[f] == nil {
				counts[f] = map[string]int{}
			}
			counts[f][string(inc)]++
		}
	}
	var g map[string]json.RawMessage
	for f, byInc := range counts {
		if exponent[f] {
			continue
		}
		if inc, ok := dominantIncrement(byInc); ok {
			if g == nil {
				g = map[string]json.RawMessage{}
			}
			g[f] = json.RawMessage(inc)
		}
	}
	return g
}

// dominantIncrement returns the most frequent non-zero increment (the
// smallest text on a tie) when it is more frequent than no change.
func dominantIncrement(byInc map[string]int) (string, bool) {
	best, bestN := "", 0
	for inc, n := range byInc {
		if inc == "0" {
			continue
		}
		if n > bestN || (n == bestN && inc < best) {
			best, bestN = inc, n
		}
	}
	return best, bestN > byInc["0"]
}

// exponentNumber reports whether raw is a JSON number in exponent form.
func exponentNumber(raw json.RawMessage) bool {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return false
	}
	return bytes.ContainsAny(raw, "eE")
}
