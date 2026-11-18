package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Policy sets (fleetpol-01a): a named document chosen by selectors. A
// database follows the highest-priority set that matches it; its own
// active document, when it has one, is an exception against that set.
// Resolution fails closed: a tie or a read error is an error (the gate's
// policy_unavailable), never the widest document.

// DefaultSetName is the set every database matches (priority 0).
const DefaultSetName = "default"

// setScopePrefix names a set's rows in sage.policy.scope.
const setScopePrefix = "policy_set:"

var (
	// ErrSetConflict: two sets of equal priority match one database.
	ErrSetConflict = errors.New("policy sets of equal priority match one database")
	// ErrInvalidSet: a set, selector, requirement or exception is malformed.
	ErrInvalidSet = errors.New("invalid policy set")
	// ErrExceptionExists: the database already has its own document.
	ErrExceptionExists = errors.New("database already has a policy exception")
)

// Selector matches a database when every criterion it names matches
// (AND); a set matches when any of its selectors does (OR). All matches
// every database. A selector with no criterion matches nothing.
type Selector struct {
	All             bool              `json:"all,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	ProviderProject string            `json:"provider_project,omitempty"`
	DiscoverySource string            `json:"discovery_source,omitempty"`
	// Creator is exact, or a prefix when it ends in "*" ("agent:*").
	Creator string `json:"creator,omitempty"`
}

// GradeRequirement asks for at least MinLetter on a review-01 dimension
// within WithinHours of enrollment.
type GradeRequirement struct {
	Dimension   string `json:"dimension"`
	MinLetter   string `json:"min_letter"`
	WithinHours int    `json:"within_hours"`
}

// SetRequirements are a set's rules beyond its document. Fleet discovery
// (fleetpol-01b) enrolls by them: TTLHours (0 is fleet.discovery.ttl_hours),
// EnrollmentDeadlineHours for the first grade, Grades as the baseline and
// Enrollment's cycle, batch and owner rule.
type SetRequirements struct {
	Grades                  []GradeRequirement     `json:"grades,omitempty"`
	OwnerRequired           bool                   `json:"owner_required,omitempty"`
	TTLHours                int                    `json:"ttl_hours,omitempty"`
	EnrollmentDeadlineHours int                    `json:"enrollment_deadline_hours,omitempty"`
	TrustCeiling            map[ChangeClass]string `json:"trust_ceiling,omitempty"`
	Enrollment              SetEnrollment          `json:"enrollment,omitzero"`
}

// PolicySet is a set's row in sage.policy_sets.
type PolicySet struct {
	Name         string          `json:"name"`
	Priority     int             `json:"priority"`
	Selectors    []Selector      `json:"selectors"`
	Requirements SetRequirements `json:"requirements"`
	CreatedBy    string          `json:"created_by,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// SetDocument is a set with its active document (nil before bootstrap).
type SetDocument struct {
	Set    PolicySet `json:"set"`
	Policy *Policy   `json:"policy,omitempty"`
}

// Member is what a selector sees of a registered database.
type Member struct {
	DatabaseID      int64             `json:"database_id"`
	Name            string            `json:"name"`
	Tags            map[string]string `json:"tags"`
	Provider        string            `json:"provider,omitempty"`
	ProviderProject string            `json:"provider_project,omitempty"`
	DiscoverySource string            `json:"discovery_source,omitempty"`
	Creator         string            `json:"creator,omitempty"`
}

// Exception is a database's own document in force over its set.
type Exception struct {
	DatabaseID    int64       `json:"database_id"`
	SetName       string      `json:"set_name"`
	Diff          []FieldDiff `json:"diff"`
	SetBy         string      `json:"set_by"`
	Reason        string      `json:"reason"`
	SetAt         time.Time   `json:"set_at"`
	ExpiresAt     *time.Time  `json:"expires_at,omitempty"`
	PolicyID      int64       `json:"policy_id"`
	PolicyVersion int64       `json:"policy_version"`
}

// Resolution is the document in force for one database and why.
type Resolution struct {
	Member Member `json:"member"`
	// Set is the set the database matches ("" when none does).
	Set    string `json:"set"`
	Policy Policy `json:"policy"`
	// Exception is set when the database's own document is in force.
	Exception *Exception `json:"exception,omitempty"`
	// ExceptionExpired: an expired exception was ignored (sweep pending).
	ExceptionExpired bool `json:"exception_expired,omitempty"`
}

// Matches reports whether every criterion the selector names matches m.
func (s Selector) Matches(m Member) bool {
	if s.All {
		return true
	}
	if !s.hasCriterion() {
		return false
	}
	for key, value := range s.Tags {
		if got, ok := m.Tags[key]; !ok || got != value {
			return false
		}
	}
	return fieldMatches(s.Provider, m.Provider) &&
		fieldMatches(s.ProviderProject, m.ProviderProject) &&
		fieldMatches(s.DiscoverySource, m.DiscoverySource) &&
		creatorMatches(s.Creator, m.Creator)
}

func (s Selector) hasCriterion() bool {
	return len(s.Tags) > 0 || s.Provider != "" || s.ProviderProject != "" ||
		s.DiscoverySource != "" || s.Creator != ""
}

func fieldMatches(want, got string) bool { return want == "" || want == got }

func creatorMatches(want, got string) bool {
	if want == "" {
		return true
	}
	if got == "" {
		return false
	}
	if prefix, ok := strings.CutSuffix(want, "*"); ok {
		return strings.HasPrefix(got, prefix)
	}
	return want == got
}

// Matches reports whether any selector matches m; no selectors match none.
func (p PolicySet) Matches(m Member) bool {
	for _, s := range p.Selectors {
		if s.Matches(m) {
			return true
		}
	}
	return false
}

// MatchSet returns the highest-priority set matching m. ok is false when
// none does; two matching sets of the top priority are ErrSetConflict.
func MatchSet(sets []PolicySet, m Member) (PolicySet, bool, error) {
	matched := make([]PolicySet, 0, 2)
	for _, s := range sets {
		if s.Matches(m) {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		return PolicySet{}, false, nil
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority > matched[j].Priority
		}
		return matched[i].Name < matched[j].Name
	})
	if len(matched) > 1 && matched[0].Priority == matched[1].Priority {
		return PolicySet{}, false, fmt.Errorf("%w: database %d matches %q and %q at "+
			"priority %d; give one a different priority", ErrSetConflict, m.DatabaseID,
			matched[0].Name, matched[1].Name, matched[0].Priority)
	}
	return matched[0], true, nil
}

// Resolve picks the document in force for member: its own document while
// its exception holds (expires_at after now), else the matching set's.
// Nothing to resolve is ErrNotFound, and so is a matching set without a
// document yet: a lower-priority set's document, which may be wider than
// the one the operator chose for this database, never stands in for it.
func Resolve(
	sets []SetDocument, member Member, own *Policy, exc *Exception, now time.Time,
) (Resolution, error) {
	byName := make(map[string]*Policy, len(sets))
	candidates := make([]PolicySet, 0, len(sets))
	for _, s := range sets {
		byName[s.Set.Name] = s.Policy
		candidates = append(candidates, s.Set)
	}
	set, ok, err := MatchSet(candidates, member)
	if err != nil {
		return Resolution{}, err
	}
	out := Resolution{Member: member, Set: set.Name}
	id := member.DatabaseID
	if own != nil {
		if exc == nil || exc.ExpiresAt == nil || now.Before(*exc.ExpiresAt) {
			out.Policy, out.Exception = *own, exceptionFor(member, set.Name, own, exc)
			out.Policy.Scope, out.Policy.Set = Scope{DatabaseID: &id}, ""
			return out, nil
		}
		out.ExceptionExpired = true
	}
	if !ok {
		return Resolution{}, ErrNotFound
	}
	if byName[set.Name] == nil {
		return Resolution{}, fmt.Errorf("%w: policy set %q has no document yet; the "+
			"sidecar gives every set one when it starts", ErrNotFound, set.Name)
	}
	// The set's row, under the database's scope: a proposal on this scope
	// becomes the database's own document (an exception), never the set's.
	out.Policy = *byName[set.Name]
	out.Policy.Scope, out.Policy.Set = Scope{DatabaseID: &id}, set.Name
	return out, nil
}

// exceptionFor names the exception in force: its stored metadata, or for
// a document without any (written before sets) who ratified it. SetName is
// the set the database matches now.
func exceptionFor(member Member, setName string, own *Policy, exc *Exception) *Exception {
	out := Exception{DatabaseID: member.DatabaseID, SetBy: own.UpdatedBy, SetAt: own.UpdatedAt,
		Reason: "per-database policy from before policy sets"}
	if exc != nil {
		out = *exc
	}
	if setName != "" {
		out.SetName = setName
	}
	out.PolicyID, out.PolicyVersion = own.ID, own.Version
	return &out
}

// setScope is the sage.policy.scope of a set's rows.
func setScope(name string) string { return setScopePrefix + name }

// DecodeStrictJSON decodes one JSON value, refusing unknown fields and
// trailing data.
func DecodeStrictJSON(raw []byte, out any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}
