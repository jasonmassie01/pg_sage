package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/findingcat"
)

// findingSourceParam is the cases query parameter that keeps only one
// subsystem's findings (rules-12b), e.g. ?finding_source=optimizer.
const findingSourceParam = "finding_source"

// unknownFindingSourceCode is the error code of a refused finding_source.
const unknownFindingSourceCode = "unknown_finding_source"

// parseFindingSource reads ?finding_source=. Absent or empty means no
// filter. A value is valid when buildSourceClause accepts it (a registry
// subsystem, or the reserved value that matches nothing); anything else,
// or more than one value, is an error, so an unfiltered list is never
// shown under a filter label.
func parseFindingSource(q url.Values) (string, error) {
	raw := q[findingSourceParam]
	if len(raw) == 0 || len(raw) == 1 && raw[0] == "" {
		return "", nil
	}
	if len(raw) == 1 {
		if _, _, ok := buildSourceClause(raw[0], 1); ok {
			return raw[0], nil
		}
	}
	return "", fmt.Errorf("unknown %s %q: want one of %s",
		findingSourceParam, strings.Join(raw, ","),
		strings.Join(findingcat.Subsystems(), ", "))
}

// casesFindingSource parses ?finding_source= and answers 400
// unknown_finding_source when it is invalid.
func casesFindingSource(w http.ResponseWriter, q url.Values) (string, bool) {
	source, err := parseFindingSource(q)
	if err != nil {
		writeChangeError(w, unknownFindingSourceCode, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return source, true
}

type findingSourceKey struct{}

// withFindingSource carries one request's finding source to its
// per-database reads; "" means no filter.
func withFindingSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, findingSourceKey{}, source)
}

// findingSourceFrom is the request's finding source, or "".
func findingSourceFrom(ctx context.Context) string {
	source, _ := ctx.Value(findingSourceKey{}).(string)
	return source
}

// addFindingSourceFields lists the subsystems the filter accepts and,
// when a source is set, says the list is filtered.
func addFindingSourceFields(body map[string]any, source string) {
	sources := findingcat.Subsystems()
	if sources == nil {
		sources = []string{}
	}
	body["finding_sources"] = sources
	if source != "" {
		body["filtered"] = map[string]string{findingSourceParam: source}
	}
}

// queryOtherCases reads one database's incident and query-hint cases and
// marks their truncation, unless the request filters by finding source:
// those cases are then left out.
func queryOtherCases(
	ctx context.Context, selected namedPool, truncated *casesTruncation,
) ([]cases.Case, error) {
	if findingSourceFrom(ctx) != "" {
		return nil, nil
	}
	incidentCases, cut, err := queryIncidentCases(ctx, selected)
	if err != nil {
		return nil, err
	}
	truncated.Incidents = cut
	hintCases, cut, err := queryHintCases(ctx, selected)
	if err != nil {
		return nil, err
	}
	truncated.QueryHints = cut
	return append(incidentCases, hintCases...), nil
}
