// Package inbox is the one list of everything waiting on a person
// (ux-01a): change approvals, plans, autonomy promotions, proposed facts,
// unsigned runbooks, agent capability requests, policy proposals and the
// sprawl census's TTL advice (fleetpol-03). It
// only reads: every decision still goes through the existing route of
// the item's own kind (the executor, the ledger, the fact store), which
// Href points at. An agent reading it decides nothing (plan-03b).
package inbox

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Kind is what a person is asked to decide.
type Kind string

// The eight kinds, in their fixed order.
const (
	KindChangeApproval    Kind = "change_approval"
	KindPlanApproval      Kind = "plan_approval"
	KindAutonomyPromotion Kind = "autonomy_promotion"
	KindFact              Kind = "fact"
	KindRunbookSignature  Kind = "runbook_signature"
	KindAgentRequest      Kind = "agent_request"
	KindPolicyException   Kind = "policy_exception"
	// KindAdvice is advice a person may act on (fleetpol-03: a TTL proposal
	// for an unused database); pg_sage takes no action on it.
	KindAdvice Kind = "advice"
)

// Kinds returns every kind in its fixed order.
func Kinds() []Kind {
	return []Kind{KindChangeApproval, KindPlanApproval, KindAutonomyPromotion, KindFact,
		KindRunbookSignature, KindAgentRequest, KindPolicyException, KindAdvice}
}

// ParseKinds reads a comma-separated kind list; "" is every kind. An
// unknown kind is an error naming the known ones. Duplicates collapse.
func ParseKinds(csv string) ([]Kind, error) {
	if strings.TrimSpace(csv) == "" {
		return Kinds(), nil
	}
	parts := strings.Split(csv, ",")
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		names = append(names, strings.TrimSpace(p))
	}
	return KindsOf(names)
}

// KindsOf validates names as kinds, in the fixed order; none is every kind.
func KindsOf(names []string) ([]Kind, error) {
	if len(names) == 0 {
		return Kinds(), nil
	}
	want := map[Kind]bool{}
	for _, n := range names {
		k := Kind(n)
		if !k.Valid() {
			return nil, fmt.Errorf("unknown inbox kind %q; known kinds: %s", n, kindList())
		}
		want[k] = true
	}
	out := make([]Kind, 0, len(want))
	for _, k := range Kinds() {
		if want[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

// Valid reports one of the eight kinds.
func (k Kind) Valid() bool {
	for _, known := range Kinds() {
		if k == known {
			return true
		}
	}
	return false
}

func kindList() string {
	names := make([]string, 0, 8)
	for _, k := range Kinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

// The risk an item carries, highest first.
const (
	RiskHigh     = "high"
	RiskModerate = "moderate"
	RiskSafe     = "safe"
)

// NormalizeRisk maps a source's risk word onto high, moderate or safe; an
// unknown or empty word is moderate (never safe: unknown is not low).
func NormalizeRisk(word string) string {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "high", "high_risk", "critical":
		return RiskHigh
	case "safe", "low":
		return RiskSafe
	}
	return RiskModerate
}

func riskRank(risk string) int {
	switch risk {
	case RiskHigh:
		return 0
	case RiskSafe:
		return 2
	}
	return 1
}

// Item is one decision waiting on a person.
type Item struct {
	Kind      Kind       `json:"kind"`
	ID        string     `json:"id"`
	Database  string     `json:"database"`
	Title     string     `json:"title"`
	Why       string     `json:"why"`
	Risk      string     `json:"risk"`
	CreatedAt time.Time  `json:"created_at"`
	DueAt     *time.Time `json:"due_at,omitempty"`
	Href      string     `json:"href"`
	Source    string     `json:"source"`
	// Class is a change approval's action class (its action type), so a
	// list can offer "select all <class> on <db>" (ux-07b); "" otherwise.
	Class string `json:"class,omitempty"`
	// Agent is the agent principal that asked for the item and Handle the
	// "<kind>:<id>" its get_request_status and wait_for_decision take
	// (mcp-01b); both "" for an item no agent raised.
	Agent  string `json:"agent,omitempty"`
	Handle string `json:"handle,omitempty"`
	// Actionable: the reader may decide it on its page. The service leaves
	// it false; the API sets it from the caller's role (CanDecide).
	Actionable bool `json:"actionable"`
}

// DeciderRole is the least role whose decision settles an item of kind:
// promotions, runbook signatures and policy ratification are admin only.
func DeciderRole(k Kind) string {
	switch k {
	case KindAutonomyPromotion, KindRunbookSignature, KindPolicyException:
		return "admin"
	}
	return "operator"
}

// CanDecide reports whether role may decide an item of kind; a viewer,
// an unknown role and an agent never can.
func CanDecide(k Kind, role string) bool {
	switch role {
	case "admin":
		return true
	case "operator":
		return DeciderRole(k) == "operator"
	}
	return false
}

// href is "#/<page>?k1=v1&k2=v2" with every value query-escaped, in the
// order given (pairs of key and value).
func href(page string, pairs ...string) string {
	var b strings.Builder
	b.WriteString("#/" + page)
	for i := 0; i+1 < len(pairs); i += 2 {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(pairs[i] + "=" + url.QueryEscape(pairs[i+1]))
	}
	return b.String()
}
