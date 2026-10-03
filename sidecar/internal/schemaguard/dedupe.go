package schemaguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// The ledger records a schema decision when it changes, not every cycle
// (dogfood lifeos: one row per invariant per cycle was 41,300 rows/hour).
// An invariant's identity is its kind, table (or family and table) and
// subject; its decision hash covers the route, disposition, reason and SQL
// of every member it covers. A decision is recorded when its hash differs
// from the one last recorded for its identity, or when the identity
// reappears after a cycle without it. The ledger is the source of truth,
// so a restart does not re-record unchanged decisions.

// DecisionRecord is one ledger row: the remediation of a representative
// member and every schema-qualified table the decision covers. Every row of
// one identity in one cycle carries the same Identity and Hash.
type DecisionRecord struct {
	Remediation
	Targets  []string
	Identity string
	Hash     string
}

// InvariantIdentity identifies an invariant across cycles. Members of one
// clone family share an identity per table and subject.
func InvariantIdentity(invariant Invariant) string {
	scope := invariant.Schema
	if invariant.Family != nil {
		scope = "family:" + invariant.Family.Key
	}
	return digest(string(invariant.Kind), scope, invariant.Table, invariant.Subject)
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:16])
}

// identityGroup is every member outcome of one identity in one cycle, in
// detection order.
type identityGroup struct {
	identity string
	items    []Remediation
}

// groupByIdentity groups outcomes by identity, keeping the order in which
// each identity was first seen.
func groupByIdentity(outcomes []Remediation) []*identityGroup {
	index := map[string]*identityGroup{}
	groups := make([]*identityGroup, 0, len(outcomes))
	for _, item := range outcomes {
		identity := InvariantIdentity(item.Invariant)
		group, ok := index[identity]
		if !ok {
			group = &identityGroup{identity: identity}
			index[identity] = group
			groups = append(groups, group)
		}
		group.items = append(group.items, item)
	}
	return groups
}

// decisionHash covers every member's outcome, sorted by target.
func decisionHash(items []Remediation) string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, strings.Join([]string{item.Invariant.Target(),
			string(item.Decision.Route), string(item.Decision.Disposition),
			item.Decision.Reason, item.Invariant.ProposedSQL}, "\x1e"))
	}
	sort.Strings(lines)
	return digest(lines...)
}

// records splits one identity's outcomes into ledger rows, one per
// distinct outcome, each listing the tables it covers.
func (g *identityGroup) records(hash string) []DecisionRecord {
	sorted := append([]Remediation(nil), g.items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Invariant.Target() < sorted[j].Invariant.Target()
	})
	index := map[string]int{}
	result := make([]DecisionRecord, 0, 1)
	for _, item := range sorted {
		key := strings.Join([]string{string(item.Decision.Route),
			string(item.Decision.Disposition), item.Decision.Reason}, "\x1e")
		position, ok := index[key]
		if !ok {
			position = len(result)
			index[key] = position
			result = append(result, DecisionRecord{Remediation: item,
				Identity: g.identity, Hash: hash})
		}
		result[position].Targets = append(result[position].Targets,
			item.Invariant.Target())
	}
	return result
}

// idleOutcome parks an invariant of an idle leftover family: nothing is
// planned or routed for a copy nobody uses.
func idleOutcome(invariant Invariant) Remediation {
	family := invariant.Family
	decision := Decision{Disposition: DispositionPark,
		Reason: fmt.Sprintf("idle leftover schema family %s (%d schemas): %s; "+
			"not remediated - drop the schemas if they are unused",
			family.Key, len(family.Members), family.Reason)}
	// An unknown kind keeps an empty class: a park needs none, and the
	// kind itself is recorded as the intent.
	if classification, err := ClassifyInvariant(invariant.Kind); err == nil {
		decision.Class = classification.Class
	}
	return Remediation{Invariant: invariant, Decision: decision}
}

// skipsIdle reports whether an invariant belongs to an idle family and is
// not backed by an owner's declared retention contract (explicit intent the
// family's idleness never overrides).
func skipsIdle(invariant Invariant) bool {
	return invariant.Family != nil && invariant.Family.Idle &&
		invariant.Kind != InvariantUnboundedAppend
}
