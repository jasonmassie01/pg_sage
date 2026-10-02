package replay

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	familyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	tagPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
)

// minCanaryLen keeps canaries distinctive enough that finding one is a
// leak, not a coincidence.
const minCanaryLen = 8

var classes = map[string]bool{ClassPositive: true, ClassConfounded: true,
	ClassMissingData: true, ClassAdversarial: true}

var statuses = map[probes.Status]bool{probes.StatusOK: true, probes.StatusEmpty: true,
	probes.StatusError: true, probes.StatusNoPrivilege: true, probes.StatusUnsupported: true}

// Validate checks a parsed case against the probe catalog and the causal
// graph: known class, gold nodes of the case's family, catalog probes
// with typed statuses, and nothing from after detection plus
// MaxLookahead (no future leakage). Every error wraps ErrInvalidCase.
func (c Case) Validate(reg *probes.Registry) error {
	if reg == nil {
		return invalid("no probe registry to validate against")
	}
	for _, check := range []func() error{c.validateHeader, c.validateGold,
		func() error { return c.validateObservations(reg) }, c.validateCanaries} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (c Case) validateHeader() error {
	switch {
	case c.Schema != Schema:
		return invalid("schema %q, want %q", c.Schema, Schema)
	case !idPattern.MatchString(c.ID):
		return invalid("id %q must match %s", c.ID, idPattern)
	case !familyPattern.MatchString(c.Family):
		return invalid("%s: family %q must match %s", c.ID, c.Family, familyPattern)
	case !classes[c.Class]:
		return invalid("%s: unknown class %q", c.ID, c.Class)
	case strings.TrimSpace(c.Description) == "":
		return invalid("%s: description is empty", c.ID)
	case strings.TrimSpace(c.Provenance) == "":
		return invalid("%s: provenance is empty", c.ID)
	case c.DetectedAt.IsZero():
		return invalid("%s: detected_at is missing", c.ID)
	}
	for _, t := range c.Tags {
		if !tagPattern.MatchString(t) {
			return invalid("%s: tag %q must match %s", c.ID, t, tagPattern)
		}
	}
	return nil
}

func (c Case) validateGold() error {
	g := c.Gold
	if strings.TrimSpace(g.Rationale) == "" {
		return invalid("%s: gold rationale is empty", c.ID)
	}
	if err := c.familyNode("root", g.Root); err != nil {
		return err
	}
	if err := c.familyNode("lookalike", g.Lookalike); err != nil {
		return err
	}
	for _, n := range g.Contributing {
		if n == "" || n == g.Root {
			return invalid("%s: contributing %q must be another node", c.ID, n)
		}
		if err := c.familyNode("contributing", n); err != nil {
			return err
		}
	}
	switch {
	case c.Class == ClassPositive && g.Root == "":
		return invalid("%s: a positive case needs a gold root", c.ID)
	case c.Class == ClassConfounded && g.Root != "":
		return invalid("%s: a confounded case has no gold root", c.ID)
	case c.Class == ClassConfounded && g.Lookalike == "":
		return invalid("%s: a confounded case names its lookalike", c.ID)
	case g.Root == "" && len(g.Contributing) > 0:
		return invalid("%s: contributing factors need a root", c.ID)
	}
	return nil
}

// familyNode checks that a named gold node is a causal-graph node of the
// case's family (empty is allowed).
func (c Case) familyNode(what, id string) error {
	if id == "" {
		return nil
	}
	n, ok := causal.NodeByID(causal.NodeID(id))
	if !ok {
		return invalid("%s: gold %s %q is not a causal graph node", c.ID, what, id)
	}
	if string(n.Family) != c.Family {
		return invalid("%s: gold %s %q belongs to family %s, not %s", c.ID, what, id,
			n.Family, c.Family)
	}
	return nil
}

func (c Case) validateObservations(reg *probes.Registry) error {
	if len(c.Observations) == 0 {
		return invalid("%s: no observations", c.ID)
	}
	for i, o := range c.Observations {
		if err := c.validateObservation(reg, o); err != nil {
			return invalid("%s: observation %d (%s): %v", c.ID, i+1, o.Probe, err)
		}
	}
	return nil
}

func (c Case) validateObservation(reg *probes.Registry, o Observation) error {
	switch _, known := reg.Spec(o.Probe); {
	case !known:
		return errors.New("unknown probe")
	case !statuses[o.Status]:
		return fmt.Errorf("unknown status %q", o.Status)
	case o.Status == probes.StatusOK && len(o.Rows) == 0:
		return errors.New("status ok needs rows")
	case o.Status != probes.StatusOK && len(o.Rows) > 0:
		return fmt.Errorf("status %s carries no rows", o.Status)
	case !o.Status.Usable() && strings.TrimSpace(o.Reason) == "":
		return fmt.Errorf("status %s needs a reason", o.Status)
	case time.Duration(o.OffsetMS)*time.Millisecond > MaxLookahead:
		return fmt.Errorf("offset %d ms is in the future of detection (more than %s)",
			o.OffsetMS, MaxLookahead)
	case time.Duration(-o.OffsetMS)*time.Millisecond > MaxLookback:
		return fmt.Errorf("offset %d ms is beyond the %s lookback", o.OffsetMS, MaxLookback)
	}
	limit := c.DetectedAt.Add(MaxLookahead)
	for _, r := range o.Rows {
		for k, v := range r {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(limit) {
				return fmt.Errorf("%s %s is in the future of detection", k, s)
			}
		}
	}
	return nil
}

func (c Case) validateCanaries() error {
	text := caseText(c)
	for _, k := range c.Canaries {
		if len(k) < minCanaryLen {
			return invalid("%s: canary %q is shorter than %d", c.ID, k, minCanaryLen)
		}
		if !strings.Contains(text, k) {
			return invalid("%s: canary %q appears nowhere in the case's data", c.ID, k)
		}
	}
	return nil
}
