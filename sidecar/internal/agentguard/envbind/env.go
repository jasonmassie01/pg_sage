// Package envbind binds every monitored database to a verified environment
// (spec §5.3, §6.5): branch, dev, stage or prod, with prod as the default.
// A label is bound to the physical identity tuple seen when it was set
// (provider resource id, system identifier, database OID, connection
// target); every evaluation re-reads the live tuple. A change, a physical
// identity seen under two labels, a branch without an active clone receipt
// or an identity too weak to verify all evaluate as prod. An agent's
// claimed environment is never an input.
package envbind

import (
	"errors"
	"fmt"
	"strconv"
)

// Env is an environment label.
type Env string

// Environments, from the widest rights to the narrowest.
const (
	EnvBranch Env = "branch"
	EnvDev    Env = "dev"
	EnvStage  Env = "stage"
	EnvProd   Env = "prod"
)

// ErrInvalidEnv is a label outside branch, dev, stage and prod.
var ErrInvalidEnv = errors.New("envbind: environment must be branch, dev, stage or prod")

var ranks = map[Env]int{EnvBranch: 0, EnvDev: 1, EnvStage: 2, EnvProd: 3}

// ParseEnv reads a label exactly as written.
func ParseEnv(s string) (Env, error) {
	if _, ok := ranks[Env(s)]; !ok {
		return "", fmt.Errorf("%w: got %q", ErrInvalidEnv, s)
	}
	return Env(s), nil
}

// Rank orders environments: branch 0 … prod 3. An unknown label ranks as
// prod, so it never widens anything.
func (e Env) Rank() int {
	if r, ok := ranks[e]; ok {
		return r
	}
	return ranks[EnvProd]
}

// Within reports whether e is at or below ceiling (spec D3: the database
// label ≤ the effective ceiling).
func (e Env) Within(ceiling Env) bool { return e.Rank() <= ceiling.Rank() }

// Widens reports whether moving from one label to another widens rights.
func Widens(from, to Env) bool { return to.Rank() < from.Rank() }

// Strength is how strongly an identity names a physical database, as
// sage.sre_database_bindings.identity_strength.
type Strength string

// Identity strengths. Only provider and cluster verify a non-prod label.
const (
	StrengthConfigured Strength = "configured"
	StrengthProvider   Strength = "provider"
	StrengthCluster    Strength = "cluster"
)

// Provider resource id sources.
const (
	RefCloudTel = "cloudtel"
	RefReceipt  = "receipt"
)

// Identity is the physical identity tuple of a monitored database (spec
// §6.5). Target (host:port/dbname) is the connection target: evidence that
// detects a re-pointed DSN, never a physical identity on its own.
// InRecovery is recorded evidence and does not change the binding.
type Identity struct {
	ProviderRef      string `json:"provider_ref,omitempty"`
	RefSource        string `json:"ref_source,omitempty"`
	SystemIdentifier string `json:"system_identifier,omitempty"`
	DBOID            uint32 `json:"db_oid,omitempty"`
	Target           string `json:"target,omitempty"`
	InRecovery       bool   `json:"in_recovery"`
}

// Strength is cluster with a readable system identifier and database OID,
// provider with a provider resource id, else configured.
func (i Identity) Strength() Strength {
	switch {
	case i.PhysicalKey() != "":
		return StrengthCluster
	case i.ProviderRef != "":
		return StrengthProvider
	}
	return StrengthConfigured
}

// PhysicalKey is "<system identifier>/<database oid>", or "" when either
// is unknown.
func (i Identity) PhysicalKey() string {
	if i.SystemIdentifier == "" || i.DBOID == 0 {
		return ""
	}
	return i.SystemIdentifier + "/" + strconv.FormatUint(uint64(i.DBOID), 10)
}

// Epoch is the value recorded as the SRE binding's cluster_epoch.
func (i Identity) Epoch() string {
	if k := i.PhysicalKey(); k != "" {
		return k
	}
	if i.ProviderRef != "" {
		return i.ProviderRef
	}
	return "unknown"
}

// Diff lists the binding fields that differ from other, in a fixed order.
// A field that became unreadable counts as a change.
func (i Identity) Diff(other Identity) []string {
	var out []string
	if i.ProviderRef != other.ProviderRef {
		out = append(out, "provider_ref")
	}
	if i.SystemIdentifier != other.SystemIdentifier {
		out = append(out, "system_identifier")
	}
	if i.DBOID != other.DBOID {
		out = append(out, "db_oid")
	}
	if i.Target != other.Target {
		out = append(out, "target")
	}
	return out
}

// distinguished reports whether provider resource ids tell two databases
// with one physical key apart: both ids known and different, or one of them
// proven by pg_sage's own clone receipt.
func distinguished(a, b Identity) bool {
	if a.ProviderRef == b.ProviderRef {
		return false
	}
	if a.RefSource == RefReceipt || b.RefSource == RefReceipt {
		return true
	}
	return a.ProviderRef != "" && b.ProviderRef != ""
}
