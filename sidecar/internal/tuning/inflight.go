package tuning

import (
	"context"
	"fmt"
	"strings"

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
		// A queued or open replacement is in flight by its build.
		if create, _, ok := optimizer.SplitIndexReplaceSQL(ddl); ok {
			ddl = create
		}
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

// overlapResult is what the overlap check found: the in-flight DDLs to
// measure alongside, the candidate's shape family, and the one existing
// index it would make redundant (a replacement, roadmap 2.3).
type overlapResult struct {
	alongside []string
	family    string
	replaces  *optimizer.IndexInfo
}

// overlap refuses a candidate an existing or in-flight index serves, one
// that would make an in-flight index redundant (nothing is built yet to
// replace), one that would make more than one existing index redundant,
// and a second candidate of a shape family (table and leading key) taken
// this cycle or in flight. A candidate that makes exactly one existing
// index redundant becomes a replacement of it.
func (v *validator) overlap(p Proposal, table, ddl string, tc optimizer.TableContext) (
	overlapResult, *Judged) {
	if v.flight.err != nil {
		j := reject(p, ReasonUnavailable, "the in-flight indexes are unreadable, so a "+
			"duplicate cannot be ruled out: %v", v.flight.err)
		return overlapResult{}, &j
	}
	inflight := v.flight.ddl[table]
	for _, def := range inflight {
		if j := overlapReason(p, ddl, def); j != nil {
			return overlapResult{}, j
		}
	}
	subsumed, j := existingOverlap(p, ddl, tc)
	if j != nil {
		return overlapResult{}, j
	}
	res := overlapResult{alongside: inflight, replaces: subsumed}
	k, ok := optimizer.LeadingKey(ddl)
	if !ok {
		return res, nil
	}
	res.family = table + "|" + k
	if other, taken := v.flight.families[res.family]; taken {
		j := reject(p, ReasonDuplicate, "one proposal per table and leading key: %s "+
			"is in flight or proposed this cycle", oneLine(other))
		return overlapResult{}, &j
	}
	return res, nil
}

// existingOverlap is the one valid existing index the candidate subsumes
// (nil for none), or a refusal: an index already serves it, or it would
// make several redundant.
func existingOverlap(p Proposal, ddl string, tc optimizer.TableContext) (
	*optimizer.IndexInfo, *Judged) {
	var subsumed []optimizer.IndexInfo
	for _, ix := range tc.Indexes {
		if !ix.IsValid {
			continue
		}
		if optimizer.Subsumes(ix.Definition, ddl) {
			j := reject(p, ReasonDuplicate, "already served by %s", oneLine(ix.Definition))
			return nil, &j
		}
		if optimizer.Subsumes(ddl, ix.Definition) {
			subsumed = append(subsumed, ix)
		}
	}
	switch len(subsumed) {
	case 0:
		return nil, nil
	case 1:
		return &subsumed[0], nil
	}
	names := make([]string, 0, len(subsumed))
	for _, ix := range subsumed {
		names = append(names, ix.Name)
	}
	j := reject(p, ReasonSubsumes, "it would make %d indexes redundant (%s); a "+
		"replacement replaces one index", len(subsumed), strings.Join(names, ", "))
	return nil, &j
}

func overlapReason(p Proposal, ddl, def string) *Judged {
	switch {
	case optimizer.Subsumes(def, ddl):
		j := reject(p, ReasonDuplicate, "already served by %s", oneLine(def))
		return &j
	case optimizer.Subsumes(ddl, def):
		j := reject(p, ReasonSubsumes, "it would make the in-flight %s redundant; it is "+
			"decided first", oneLine(def))
		return &j
	}
	return nil
}

// errNoInFlight marks a validator never prepared for index creates.
var errNoInFlight = fmt.Errorf("in-flight indexes were not read")
