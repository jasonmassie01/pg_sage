// Package agentguard is pg_sage's agent governance core (AGENTDB-SPEC §6.4,
// §6.6): agent principals and their accountable sponsors, the MCP tokens
// bound to them, the request plumbing that carries a principal, and the
// two cluster roles every principal gets (sage_agent_<id10> for the direct
// lane and sage_agentb_<id10> for the brokered lane).
//
// Roles are created, altered and retired only through the executor
// (Executor.Apply) as the typed contracts guard_role_ensure and
// guard_role_retire, operator-approved (L2) in G1. Agent roles are never
// superuser, never BYPASSRLS, hold no dangerous membership and own nothing.
//
// Principals, cluster roles, their sealed broker credentials and taint
// live in the control database; the PUBLIC baseline and provenance live in
// each monitored database.
package agentguard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Env is a database environment label (§5.3). Order: branch < dev <
// stage < prod; a database is allowed when its label is at most the
// principal's ceiling.
type Env string

// Environments.
const (
	EnvBranch Env = "branch"
	EnvDev    Env = "dev"
	EnvStage  Env = "stage"
	EnvProd   Env = "prod"
)

var envRank = map[Env]int{EnvBranch: 1, EnvDev: 2, EnvStage: 3, EnvProd: 4}

// Valid reports whether e is one of the four labels.
func (e Env) Valid() bool { return envRank[e] > 0 }

// Rank orders environments (0 for an invalid one).
func (e Env) Rank() int { return envRank[e] }

// MinEnv is the narrower of two ceilings; an invalid one counts as branch
// (the narrowest), so a bad value never widens.
func MinEnv(a, b Env) Env {
	switch {
	case !a.Valid() || !b.Valid():
		return EnvBranch
	case a.Rank() <= b.Rank():
		return a
	default:
		return b
	}
}

// Status is a principal's lifecycle state.
type Status string

// Statuses.
const (
	StatusActive  Status = "active"
	StatusFrozen  Status = "frozen"
	StatusRetired Status = "retired"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	return s == StatusActive || s == StatusFrozen || s == StatusRetired
}

// Principal is one agent identity (§6.4). SponsorActive and Tainted are
// loaded with it: they are the D2 and D8 inputs of the gate.
type Principal struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	SponsorUserID *int      `json:"sponsor_user_id"`
	Tenant        string    `json:"tenant"`
	Profile       string    `json:"profile"`
	EnvCeiling    Env       `json:"env_ceiling"`
	Status        Status    `json:"status"`
	FrozenReason  string    `json:"frozen_reason,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// SponsorActive is true when the sponsor is an existing sage.users row.
	SponsorActive bool `json:"sponsor_active"`
	// Tainted is true while an uncleared taint row exists (§6.10).
	Tainted bool `json:"tainted"`
}

// Sponsored reports whether p has an accountable, active sponsor (ID-2,
// D2). An unsponsored principal is at L0 for agent_* tools.
func (p Principal) Sponsored() bool { return p.SponsorUserID != nil && p.SponsorActive }

// Frozen reports whether p is frozen (D1).
func (p Principal) Frozen() bool { return p.Status == StatusFrozen }

// Retired reports whether p is retired; a retired principal's tokens no
// longer authenticate.
func (p Principal) Retired() bool { return p.Status == StatusRetired }

// LoginRole is p's direct-lane role, sage_agent_<id10>.
func (p Principal) LoginRole() string { return LoginRoleName(p.ID) }

// BrokerRole is p's brokered-lane role, sage_agentb_<id10>.
func (p Principal) BrokerRole() string { return BrokerRoleName(p.ID) }

var (
	idPattern   = regexp.MustCompile(`^agp_[a-z2-7]{20}$`)
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	// RolePattern matches both agent role names (§6.6).
	RolePattern = regexp.MustCompile(`^sage_agentb?_[a-z2-7]{10}$`)
)

// RoleRegex is RolePattern's source, for SQL (usename ~ RoleRegex). It is
// the single definition of an agent role name (§6.6: 10 lower base32
// characters of sha256(principal id)); the spec's kill runbook uses it.
const RoleRegex = `^sage_agentb?_[a-z2-7]{10}$`

// IsAgentRoleName reports whether name is an agent role name, of either
// lane. Attribution, posture and the broker should all use it.
func IsAgentRoleName(name string) bool { return RolePattern.MatchString(name) }

// lowerBase32 is RFC 4648 base32 in lower case, without padding.
var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").
	WithPadding(base32.NoPadding)

// ValidID reports whether id is a principal id: "agp_" + 20 lower base32.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidName reports whether name is a principal slug.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// NewID returns a random principal id.
func NewID() (string, error) {
	s, err := randomBase32(20)
	if err != nil {
		return "", err
	}
	return "agp_" + s, nil
}

// randomBase32 returns n random lower base32 characters.
func randomBase32(n int) (string, error) {
	buf := make([]byte, (n*5+7)/8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("agentguard: random id: %w", err)
	}
	return lowerBase32.EncodeToString(buf)[:n], nil
}

// roleSuffix is 10 lower base32 characters of sha256(principal id).
func roleSuffix(principalID string) string {
	sum := sha256.Sum256([]byte(principalID))
	return strings.ToLower(lowerBase32.EncodeToString(sum[:]))[:10]
}

// LoginRoleName is the direct-lane role of a principal id.
func LoginRoleName(principalID string) string { return "sage_agent_" + roleSuffix(principalID) }

// BrokerRoleName is the brokered-lane role of a principal id.
func BrokerRoleName(principalID string) string { return "sage_agentb_" + roleSuffix(principalID) }
