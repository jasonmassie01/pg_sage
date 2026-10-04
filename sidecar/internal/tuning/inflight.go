package tuning

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Index candidates are judged against the indexes of their table that
// exist or are in flight (queued in sage.action_queue, or open proposals
// awaiting a decision), lifeos 1.10.0: an index and, ten minutes later,
// the same keys with a weaker predicate were each verified on their own.

// inFlight are the in-flight index DDLs by canonical table, and the shape
// families (table and leading key) already taken this cycle.
type inFlight struct {
	ddl      map[string][]string
	families map[string]string
	err      error
}

// loadInFlight reads the queued index creates and adds the open index
// proposals.
func (v *validator) loadInFlight(ctx context.Context, open []analyzer.Finding) {
	v.flight = inFlight{ddl: map[string][]string{}, families: map[string]string{}}
	queued, err := v.a.deps.Store.InFlightIndexes(ctx)
	if err != nil {
		v.flight.err = err
		return
	}
	for _, f := range open {
		if isIndexCreate(f) && f.RecommendedSQL != "" {
			queued = append(queued, f.RecommendedSQL)
		}
	}
	for _, ddl := range queued {
		spec, err := optimizer.ParseIndexDDL(ddl)
		if err != nil || spec.TableSchema == "" {
			continue
		}
		t := qualified(spec.TableSchema, spec.TableName)
		v.flight.ddl[t] = append(v.flight.ddl[t], ddl)
		if k, ok := optimizer.LeadingKey(ddl); ok {
			v.flight.families[t+"|"+k] = ddl
		}
	}
}

// overlap refuses a candidate an existing or in-flight index serves, one
// that would make such an index redundant (a replacement is a create and a
// drop decided together, never an independent create), and a second
// candidate of a shape family (table and leading key) taken this cycle or
// in flight. It returns the in-flight DDLs to measure alongside and the
// candidate's family, taken once it is admitted.
func (v *validator) overlap(p Proposal, table, ddl string, tc optimizer.TableContext) (
	[]string, string, *Judged) {
	if v.flight.err != nil {
		j := reject(p, ReasonUnavailable, "the in-flight indexes are unreadable, so a "+
			"duplicate cannot be ruled out: %v", v.flight.err)
		return nil, "", &j
	}
	inflight := v.flight.ddl[table]
	defs := append([]string(nil), inflight...)
	for _, ix := range tc.Indexes {
		if ix.IsValid {
			defs = append(defs, ix.Definition)
		}
	}
	for _, def := range defs {
		if j := overlapReason(p, ddl, def); j != nil {
			return nil, "", j
		}
	}
	k, ok := optimizer.LeadingKey(ddl)
	if !ok {
		return inflight, "", nil
	}
	family := table + "|" + k
	if other, taken := v.flight.families[family]; taken {
		j := reject(p, ReasonDuplicate, "one proposal per table and leading key: %s "+
			"is in flight or proposed this cycle", oneLine(other))
		return nil, "", &j
	}
	return inflight, family, nil
}

func overlapReason(p Proposal, ddl, def string) *Judged {
	switch {
	case optimizer.Subsumes(def, ddl):
		j := reject(p, ReasonDuplicate, "already served by %s", oneLine(def))
		return &j
	case optimizer.Subsumes(ddl, def):
		j := reject(p, ReasonSubsumes, "it would make %s redundant; a replacement "+
			"(create and drop decided together) is not an independent create",
			oneLine(def))
		return &j
	}
	return nil
}

// errNoInFlight marks a validator never prepared for index creates.
var errNoInFlight = fmt.Errorf("in-flight indexes were not read")
