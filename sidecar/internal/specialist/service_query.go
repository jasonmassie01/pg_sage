package specialist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The caller's statement scope in the service: resolved after admission
// (a hash through the database's pg_stat_statements), it makes a
// plan-regression investigation about the statement, keeps attaches and
// window matches to investigations of the same statement, is recorded
// with the caller's request and echoed back to that caller only.

// resolvedQuery is a caller's statement: its queryid and the hash it sent.
type resolvedQuery struct {
	id   int64
	hash string
}

// resolveQuery resolves req's scope against b; nil when there is none.
func resolveQuery(ctx context.Context, b Backend, req OpenRequest) (*resolvedQuery, error) {
	if req.QueryID == "" && req.QueryHash == "" {
		return nil, nil
	}
	q := &resolvedQuery{hash: req.QueryHash}
	if req.QueryID != "" {
		id, err := req.QueryID.Int64()
		if err != nil {
			return nil, invalidf("%v", err)
		}
		q.id = id
	}
	if req.QueryHash == "" {
		return q, nil
	}
	ids, err := b.ResolveQueryHash(ctx, req.QueryHash)
	var coded *codedError
	switch {
	case errors.As(err, &coded):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: resolving query_hash: %v", ErrUnavailable, err)
	case len(ids) == 0:
		return nil, invalidf("query_hash matches no statement in pg_stat_statements of " +
			"this database")
	case q.id != 0 && !slices.Contains(ids, q.id):
		return nil, invalidf("query_hash and query_id name different statements")
	case q.id == 0 && len(ids) > 1:
		return nil, invalidf("query_hash matches %d statements; pass query_id to choose "+
			"one", len(ids))
	case q.id == 0:
		q.id = ids[0]
	}
	return q, nil
}

// subjectFor is the subject a new investigation of family opens with: a
// plan regression scoped to a statement is about that statement.
func (q *resolvedQuery) subjectFor(family string) string {
	if q != nil && triggerFor(family) == sre.TriggerPlan {
		return sre.QuerySubject(q.id)
	}
	return externalSubject
}

// planSubject is the subject a window match must have ("" matches any).
func (q *resolvedQuery) planSubject(family string) string {
	if q != nil && triggerFor(family) == sre.TriggerPlan {
		return sre.QuerySubject(q.id)
	}
	return ""
}

// checkAttach refuses an attach to a plan investigation about another
// statement.
func (q *resolvedQuery) checkAttach(inv sre.Investigation) error {
	if q == nil || inv.TriggerKind != sre.TriggerPlan {
		return nil
	}
	if other, ok := sre.ParseQuerySubject(inv.Subject); ok && other != q.id {
		return invalidf("the investigation is about queryid %d, not the statement this "+
			"request names", other)
	}
	return nil
}

// scope is the echoed scope of q on inv; nil without a scope.
func (q *resolvedQuery) scope(inv sre.Investigation) *QueryScope {
	if q == nil {
		return nil
	}
	return &QueryScope{QueryID: strconv.FormatInt(q.id, 10), QueryHash: q.hash,
		Applied: appliedOn(inv, q.id)}
}

// appliedOn is how a statement scope applies to an investigation.
func appliedOn(inv sre.Investigation, id int64) string {
	if inv.TriggerKind == sre.TriggerPlan && inv.Subject == sre.QuerySubject(id) {
		return QueryAppliedProbes
	}
	return QueryAppliedEvidence
}

// queryScope maps the caller's recorded scope onto a result: whether the
// root is about the statement and which evidence mentions it.
func (m mapper) queryScope(rec *Record, d sre.Detail) *QueryScopeResult {
	if rec == nil || rec.Query == nil {
		return nil
	}
	id, err := QueryID(rec.Query.QueryID).Int64()
	if err != nil {
		return nil
	}
	out := &QueryScopeResult{QueryID: rec.Query.QueryID, QueryHash: rec.Query.QueryHash,
		Applied: appliedOn(d.Investigation, id), EvidenceIDs: []string{}}
	subject := sre.QuerySubject(id)
	for _, h := range sre.LatestRevision(d.Hypotheses) {
		if h.Status == sre.HypothesisRoot && h.Subject == subject {
			out.RootMatches = true
		}
	}
	for _, e := range d.Evidence {
		if mentionsQuery(e.Payload, rec.Query.QueryID) {
			out.EvidenceIDs = append(out.EvidenceIDs, string(e.ID))
		}
	}
	return out
}

// mentionsQuery reports whether an evidence payload has a row whose
// queryid (or query_id) is id, compared exactly as decimal text.
func mentionsQuery(payload []byte, id string) bool {
	var p struct {
		Rows []map[string]any `json:"rows"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if len(payload) == 0 || dec.Decode(&p) != nil {
		return false
	}
	for _, row := range p.Rows {
		for _, col := range []string{"queryid", "query_id"} {
			switch v := row[col].(type) {
			case json.Number:
				if v.String() == id {
					return true
				}
			case string:
				if v == id {
					return true
				}
			}
		}
	}
	return false
}
