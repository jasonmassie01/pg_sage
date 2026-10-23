// Package principalfile is the declarative, versioned file for agent
// principals (E2, PC-1 policies as code, AP-5 GitOps path), with
// terraform-like plan and apply:
//
//	apiVersion: pg_sage/v1
//	kind: AgentPrincipals
//	metadata: {name: prod-agents}     # the file's identity; it owns what it creates
//	spec:
//	  prune: false                     # retire principals it created and no longer lists
//	  principals:
//	    - name: ci-bot                 # ^[a-z][a-z0-9-]{1,62}$
//	      sponsor: alice@example.com   # a pg_sage user; empty = unsponsored (L0)
//	      profile: readonly-analyst    # agents.profiles
//	      env_ceiling: stage           # branch | dev | stage | prod (default dev)
//	      tenant: ""
//	      status: active               # active | frozen
//	      identities:                  # E2 identity bindings (iss, sub)
//	        - {issuer: https://idp.example.com, subject: 0oa1}
//
// Profiles and L3 envelopes have reserved keys (profiles, envelopes) that
// are refused until G2 defines them. A plan records hashes of the file and
// of the principal state it was made against; apply refuses a plan whose
// file or state has changed since, refuses widening without explicit
// approval, and applies narrowing changes first. Principals are never
// deleted: removal retires. The core workstream implements Backend over
// its principal store.
package principalfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"

	"gopkg.in/yaml.v3"
)

// API version and kind of the file.
const (
	APIVersion = "pg_sage/v1"
	Kind       = "AgentPrincipals"
)

var slug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// envRank orders environments by how much they expose.
var envRank = map[string]int{"branch": 0, "dev": 1, "stage": 2, "prod": 3}

// File is a parsed principals file.
type File struct {
	APIVersion string   `yaml:"apiVersion" json:"api_version"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

// Metadata names the file.
type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

// Spec is what the file declares.
type Spec struct {
	Prune      bool            `yaml:"prune" json:"prune"`
	Principals []PrincipalSpec `yaml:"principals" json:"principals"`
	// Profiles and Envelopes are reserved for G2; a file setting them is
	// refused rather than silently ignored.
	Profiles  map[string]any `yaml:"profiles" json:"-"`
	Envelopes []any          `yaml:"envelopes" json:"-"`
}

// PrincipalSpec is one declared principal.
type PrincipalSpec struct {
	Name       string     `yaml:"name" json:"name"`
	Sponsor    string     `yaml:"sponsor" json:"sponsor"`
	Profile    string     `yaml:"profile" json:"profile"`
	EnvCeiling string     `yaml:"env_ceiling" json:"env_ceiling"`
	Tenant     string     `yaml:"tenant" json:"tenant"`
	Status     string     `yaml:"status" json:"status"`
	Identities []Identity `yaml:"identities" json:"identities"`
}

// Identity is an external identity bound to a principal.
type Identity struct {
	Issuer  string `yaml:"issuer" json:"issuer"`
	Subject string `yaml:"subject" json:"subject"`
}

// Parse reads and validates a principals file, reporting every problem.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("principals file: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("principals file: must hold exactly one YAML document")
	}
	for i := range f.Spec.Principals {
		p := &f.Spec.Principals[i]
		if p.Status == "" {
			p.Status = "active"
		}
		if p.EnvCeiling == "" {
			p.EnvCeiling = "dev"
		}
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) validate() error {
	var errs []error
	if f.APIVersion != APIVersion || f.Kind != Kind {
		errs = append(errs, fmt.Errorf("apiVersion/kind must be %s/%s, got %q/%q",
			APIVersion, Kind, f.APIVersion, f.Kind))
	}
	if !slug.MatchString(f.Metadata.Name) {
		errs = append(errs, fmt.Errorf("metadata.name %q must match %s", f.Metadata.Name,
			slug))
	}
	if f.Spec.Profiles != nil || f.Spec.Envelopes != nil {
		errs = append(errs, fmt.Errorf("spec.profiles and spec.envelopes are not "+
			"supported until G2; declare profiles in agents.profiles"))
	}
	names, idents := map[string]bool{}, map[Identity]string{}
	for _, p := range f.Spec.Principals {
		errs = append(errs, p.validate()...)
		if names[p.Name] {
			errs = append(errs, fmt.Errorf("principal %q is declared twice", p.Name))
		}
		names[p.Name] = true
		for _, id := range p.Identities {
			if owner, dup := idents[id]; dup {
				errs = append(errs, fmt.Errorf("identity %s|%s is bound to both %q and %q",
					id.Issuer, id.Subject, owner, p.Name))
			}
			idents[id] = p.Name
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("principals file: %w", errors.Join(errs...))
	}
	return nil
}

func (p PrincipalSpec) validate() []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("principal %q: "+format, append([]any{p.Name},
			args...)...))
	}
	if !slug.MatchString(p.Name) {
		bad("name must match %s", slug)
	}
	if p.Profile == "" {
		bad("profile is required")
	}
	if _, ok := envRank[p.EnvCeiling]; !ok {
		bad("env_ceiling %q must be branch, dev, stage or prod", p.EnvCeiling)
	}
	if p.Status != "active" && p.Status != "frozen" {
		bad("status %q must be active or frozen (retire by removing it, with prune)",
			p.Status)
	}
	for _, id := range p.Identities {
		u, err := url.Parse(id.Issuer)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			bad("identity issuer %q must be an https URL", id.Issuer)
		}
		if id.Subject == "" || len(id.Subject) > 255 {
			bad("identity subject must be 1-255 characters")
		}
	}
	return errs
}
