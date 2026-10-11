package agentposture

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// detectorID is the id every detector carries: AP and two digits.
var detectorID = regexp.MustCompile(`^AP-[0-9]{2}$`)

// Registry holds detectors by id. It is safe for concurrent use.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Detector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byID: map[string]Detector{}} }

var defaultRegistry = NewRegistry()

// Default is the registry the shipped detectors register into; the first
// look and the monitor run it unless given another.
func Default() *Registry { return defaultRegistry }

// Register adds d to the default registry. It is called from detector
// init functions, so an invalid detector is a programming error: it
// panics.
func Register(d Detector) {
	if err := defaultRegistry.Register(d); err != nil {
		panic(err)
	}
}

// Register adds d after validating its spec; an invalid spec or a
// duplicate id is ErrInvalidDetector.
func (r *Registry) Register(d Detector) error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidDetector)
	}
	if d == nil {
		return fmt.Errorf("%w: nil detector", ErrInvalidDetector)
	}
	spec := d.Spec()
	if err := validateSpec(spec); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[spec.ID]; dup {
		return fmt.Errorf("%w: %s is already registered", ErrInvalidDetector, spec.ID)
	}
	r.byID[spec.ID] = d
	return nil
}

func validateSpec(s Spec) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalidDetector, s.ID, fmt.Sprintf(format, args...))
	}
	if !detectorID.MatchString(s.ID) {
		return bad("id must match AP-NN")
	}
	if s.Title == "" {
		return bad("no title")
	}
	if !s.Severity.Valid() {
		return bad("severity %q is not info, warning or critical", s.Severity)
	}
	seen := map[string]bool{}
	for _, a := range s.Arms {
		switch {
		case a.Name == "":
			return bad("an arm has no name")
		case seen[a.Name]:
			return bad("arm %s is declared twice", a.Name)
		case a.MinVersion > 0 && a.MaxVersion > 0 && a.MinVersion >= a.MaxVersion:
			return bad("arm %s has an empty version range", a.Name)
		case a.bounded() && a.SkipReason == "":
			return bad("arm %s has no skip reason", a.Name)
		}
		seen[a.Name] = true
	}
	return nil
}

// Detectors lists the registered detectors by id.
func (r *Registry) Detectors() []Detector {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Detector, 0, len(r.byID))
	for _, d := range r.byID {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec().ID < out[j].Spec().ID })
	return out
}

// Get returns the detector registered under id.
func (r *Registry) Get(id string) (Detector, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	return d, ok
}
