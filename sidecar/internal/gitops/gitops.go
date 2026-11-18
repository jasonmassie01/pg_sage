// Package gitops reconciles a directory of YAML files (policy.directory,
// cli-02a) into pg_sage's own policy sets, agent principals and
// per-database grants, so a team can review those changes in git.
//
// The reconciler owns only the rows it made or adopted (managed_by =
// 'config', or a grant's source = 'config'); rows made through the API
// are never changed. Every write goes through the policy, agentguard and
// auth stores in one transaction with one audit entry. A change that
// narrows applies with the file as actor; one that widens what an agent
// or pg_sage may do is only proposed and waits for a person.
package gitops

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Modes (policy.directory_mode).
const (
	ModeDryRun = "dry_run"
	ModeApply  = "apply"
)

// The directory's layout: one subdirectory, and one kind, per row type.
const (
	DirPolicySets = "policy-sets"
	DirPrincipals = "principals"
	DirGrants     = "grants"

	KindPolicySetFile = "PolicySet"
	KindPrincipalFile = "Principal"
	KindGrantsFile    = "DatabaseGrants"

	// SchemaVersion is the only schema_version a file may name.
	SchemaVersion = 1
	// MaxFileBytes bounds one file (1 MiB).
	MaxFileBytes = 1 << 20
)

// ErrDirectoryMissing is a policy.directory that does not exist: a
// startup warning, and nothing is reconciled (nothing is retired either).
var ErrDirectoryMissing = errors.New("gitops: policy.directory does not exist")

// Problem is one invalid thing in a file, at its line (0: the file).
type Problem struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	if p.Line <= 0 {
		return p.File + ": " + p.Message
	}
	return fmt.Sprintf("%s:%d: %s", p.File, p.Line, p.Message)
}

// InvalidError is a directory with problems: the reconcile writes nothing.
type InvalidError struct{ Problems []Problem }

func (e *InvalidError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return "policy.directory is invalid, nothing was reconciled: " + strings.Join(parts, "; ")
}

// Source is where a spec came from: its file (relative, with slashes),
// the line of its name and the file's sha256.
type Source struct {
	Path string
	Line int
	Hash string
	// lines are the line of each key the spec set.
	lines map[string]int
}

// Actor is the identity a write from this file is recorded under.
func (s Source) Actor() string { return "config:" + s.Path + "@" + s.Hash }

// lineOf is the line of key, else the spec's own line.
func (s Source) lineOf(key string) int {
	if l, ok := s.lines[key]; ok {
		return l
	}
	return s.Line
}

func (s Source) problem(key, format string, args ...any) Problem {
	return Problem{File: s.Path, Line: s.lineOf(key), Message: fmt.Sprintf(format, args...)}
}

// SetSpec is one policy-set file.
type SetSpec struct {
	Source
	File  policy.SetFile
	Adopt bool
}

// PrincipalSpec is one principal file. Sponsor is an email ("" leaves the
// stored sponsor alone).
type PrincipalSpec struct {
	Source
	Name       string
	Tenant     string
	Profile    string
	EnvCeiling agentguard.Env
	Sponsor    string
	Adopt      bool
}

// GrantSpec is one entry of a grants file; User is a lower-cased email.
type GrantSpec struct {
	Source
	User     string
	Database string
	Role     string
	Adopt    bool
}

// Directory is a loaded, validated directory and its content hash.
type Directory struct {
	Root       string
	Hash       string
	Sets       []SetSpec
	Principals []PrincipalSpec
	Grants     []GrantSpec
}

// Row kinds and operations of a Change.
const (
	KindPolicySet = "policy_set"
	KindPrincipal = "principal"
	KindGrant     = "grant"

	OpCreate = "create"
	OpUpdate = "update"
	OpAdopt  = "adopt"
	OpRetire = "retire"
	// OpRatify applies a narrower policy document.
	OpRatify = "ratify"
	// OpPropose files a widening for a person to ratify.
	OpPropose = "propose"
	// OpHold is a change that would widen and has no proposal flow; it
	// is not written.
	OpHold = "hold"
	// OpWaiting is a widening already waiting for a person.
	OpWaiting = "waiting"
)

// Change is one row the reconcile writes, proposes or holds.
type Change struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Op     string `json:"op"`
	File   string `json:"file,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// writes reports whether the change writes a row.
func (c Change) writes() bool { return c.Op != OpHold && c.Op != OpWaiting }

// Result is one reconcile: its diff and whether it was written.
type Result struct {
	Mode     string   `json:"mode"`
	Hash     string   `json:"hash"`
	Changes  []Change `json:"changes"`
	Applied  bool     `json:"applied"`
	Follower bool     `json:"follower,omitempty"`
}

// Writes counts the changes that write a row.
func (r Result) Writes() int {
	n := 0
	for _, c := range r.Changes {
		if c.writes() {
			n++
		}
	}
	return n
}
