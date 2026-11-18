package policy

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	setNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	tagKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,128}$`)
	gradeLetters   = []string{"A", "B", "C", "D", "F"}
	trustLevels    = []string{TrustObservation, TrustAdvisory, TrustAutonomous}
)

// Bounds on a set's fields.
const (
	maxSetPriority  = 1000
	maxSetSelectors = 50
	maxSetHours     = 87600 // ten years
)

// DecodeSet decodes a set from JSON, refusing unknown fields at every
// level (an unknown selector key included). It does not validate values.
func DecodeSet(raw []byte) (PolicySet, error) {
	var set PolicySet
	if len(strings.TrimSpace(string(raw))) == 0 {
		return PolicySet{}, fmt.Errorf("%w: empty body", ErrInvalidSet)
	}
	if err := DecodeStrictJSON(raw, &set); err != nil {
		return PolicySet{}, fmt.Errorf("%w: %v", ErrInvalidSet, err)
	}
	return set, nil
}

// ValidateSet checks a set's name, priority, selectors and requirements.
// dimensions are the review rubric's dimension ids a grade may name.
func ValidateSet(set PolicySet, dimensions []string) error {
	if !setNamePattern.MatchString(set.Name) {
		return invalidSet("name %q must be 1-63 of a-z, 0-9, _ and -, starting with a "+
			"letter", set.Name)
	}
	if err := validateSetPriority(set); err != nil {
		return err
	}
	if len(set.Selectors) > maxSetSelectors {
		return invalidSet("%d selectors, at most %d", len(set.Selectors), maxSetSelectors)
	}
	for i, s := range set.Selectors {
		if err := validateSelector(s); err != nil {
			return invalidSet("selector %d: %v", i, err)
		}
	}
	return validateRequirements(set.Requirements, dimensions)
}

func validateSetPriority(set PolicySet) error {
	if set.Name == DefaultSetName {
		if set.Priority != 0 || len(set.Selectors) != 1 || !set.Selectors[0].All ||
			set.Selectors[0].hasCriterion() {
			return invalidSet("the default set keeps priority 0 and the single selector " +
				"{all: true}, so every database resolves to a document")
		}
		return nil
	}
	if set.Priority < 1 || set.Priority > maxSetPriority {
		return invalidSet("priority %d must be 1-%d (0 is the default set's)",
			set.Priority, maxSetPriority)
	}
	return nil
}

func validateSelector(s Selector) error {
	if s.All && s.hasCriterion() {
		return fmt.Errorf("all matches every database and takes no other criterion")
	}
	if !s.All && !s.hasCriterion() {
		return fmt.Errorf("a selector needs at least one criterion")
	}
	for key, value := range s.Tags {
		if !tagKeyPattern.MatchString(key) {
			return fmt.Errorf("tag key %q must be 1-128 of A-Z, a-z, 0-9, _ . / -", key)
		}
		if len(value) > 256 {
			return fmt.Errorf("tag %q value is longer than 256", key)
		}
	}
	for _, v := range []string{s.Provider, s.ProviderProject, s.DiscoverySource, s.Creator} {
		if len(v) > 256 {
			return fmt.Errorf("a selector value is longer than 256")
		}
	}
	return nil
}

func validateRequirements(req SetRequirements, dimensions []string) error {
	for _, g := range req.Grades {
		if !slices.Contains(dimensions, g.Dimension) {
			return invalidSet("grade requirement on unknown dimension %q (known: %s)",
				g.Dimension, strings.Join(dimensions, ", "))
		}
		if !slices.Contains(gradeLetters, g.MinLetter) {
			return invalidSet("grade requirement on %s: min_letter %q must be one of %s",
				g.Dimension, g.MinLetter, strings.Join(gradeLetters, ", "))
		}
		if g.WithinHours < 1 || g.WithinHours > maxSetHours {
			return invalidSet("grade requirement on %s: within_hours %d must be 1-%d",
				g.Dimension, g.WithinHours, maxSetHours)
		}
	}
	if req.TTLHours < 0 || req.TTLHours > maxSetHours {
		return invalidSet("ttl_hours %d must be 0-%d", req.TTLHours, maxSetHours)
	}
	if req.EnrollmentDeadlineHours < 0 || req.EnrollmentDeadlineHours > maxSetHours {
		return invalidSet("enrollment_deadline_hours %d must be 0-%d",
			req.EnrollmentDeadlineHours, maxSetHours)
	}
	known := knownChangeClasses()
	for class, level := range req.TrustCeiling {
		if !known[class] {
			return invalidSet("trust ceiling on unknown change class %q", class)
		}
		if !slices.Contains(trustLevels, level) {
			return invalidSet("trust ceiling on %s: level %q must be one of %s",
				class, level, strings.Join(trustLevels, ", "))
		}
	}
	return nil
}

func invalidSet(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSet, fmt.Sprintf(format, args...))
}

// CheckTies refuses sets that would tie on a registered member, and two
// sets of equal priority where either matches every database.
func CheckTies(sets []PolicySet, members []Member) error {
	for i, a := range sets {
		for _, b := range sets[i+1:] {
			if a.Priority == b.Priority && (matchesAll(a) || matchesAll(b)) &&
				len(a.Selectors) > 0 && len(b.Selectors) > 0 {
				return fmt.Errorf("%w: %q and %q share priority %d and one matches every "+
					"database", ErrSetConflict, a.Name, b.Name, a.Priority)
			}
		}
	}
	for _, m := range members {
		if _, _, err := MatchSet(sets, m); err != nil {
			return err
		}
	}
	return nil
}

func matchesAll(set PolicySet) bool {
	for _, s := range set.Selectors {
		if s.All {
			return true
		}
	}
	return false
}
