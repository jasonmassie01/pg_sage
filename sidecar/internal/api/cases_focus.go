package api

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// maxCaseIdentityLen bounds ?case=; a finding identity carries an object
// identifier of any length, so it is generous.
const maxCaseIdentityLen = 4096

// caseFocusLimit bounds the rows one ?case= lookup reads.
const caseFocusLimit = 10

// findingObjectKeySQL is the object part of a finding's case identity
// (cases.IdentityKeyForFinding): the object identifier.
const findingObjectKeySQL = `COALESCE(object_identifier, '')`

// caseFocus reports a ?case= lookup: found says the case is in the
// response, loaded that it was read separately because the page cut it.
type caseFocus struct {
	Case   string `json:"case"`
	Found  bool   `json:"found"`
	Loaded bool   `json:"loaded"`
}

var caseIdentityKinds = []string{"finding", "incident", "query_hint"}

func validCaseIdentity(identity string) bool {
	if len(identity) > maxCaseIdentityLen {
		return false
	}
	kind, rest, ok := strings.Cut(identity, ":")
	if !ok || rest == "" {
		return false
	}
	for _, k := range caseIdentityKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// addFocusedCase makes sure the case named by identity is in the report
// when it still exists, reading it from its database when the capped page
// did not carry it (cases-05).
func addFocusedCase(
	ctx context.Context, mgr *fleet.DatabaseManager, database string,
	report *casesReport, identity string,
) caseFocus {
	focus := caseFocus{Case: identity}
	for _, c := range report.Cases {
		if c.ID == identity {
			focus.Found = true
			return focus
		}
	}
	if mgr == nil {
		return focus
	}
	// One deadline for the whole lookup: an incident link that misses can
	// try every database, and that must not cost one deadline each.
	lookupCtx, cancel := context.WithTimeout(ctx, casesDatabaseTimeout)
	defer cancel()
	for _, sel := range focusCandidates(mgr, database, *report, identity) {
		found, ok, err := lookupCase(lookupCtx, mgr, sel, identity)
		if err != nil {
			slog.Warn("focused case read failed", "database", sel.name,
				"error_class", classifyCasesError(err), "error", err)
			continue
		}
		if ok {
			report.Cases = append(report.Cases, found)
			sortCasesGlobally(report.Cases)
			focus.Found, focus.Loaded = true, true
			return focus
		}
	}
	return focus
}

// focusCandidates lists the readable databases that can hold the case:
// the one its identity names, and for an incident (whose identity carries
// the stored database name, not always the fleet alias) every other one.
func focusCandidates(
	mgr *fleet.DatabaseManager, database string, report casesReport, identity string,
) []namedPool {
	down := map[string]bool{}
	for _, u := range report.Unavailable {
		down[u.Database] = true
	}
	parts := strings.SplitN(identity, ":", 3)
	named := ""
	if len(parts) == 3 {
		named = parts[1]
	}
	var first, rest []namedPool
	for _, sel := range poolsForDatabaseSelection(mgr, database) {
		switch {
		case down[sel.name]:
		case sel.name == named:
			first = append(first, sel)
		default:
			rest = append(rest, sel)
		}
	}
	if parts[0] != "incident" {
		return first
	}
	return append(first, rest...)
}

func lookupCase(
	ctx context.Context, mgr *fleet.DatabaseManager, sel namedPool, identity string,
) (cases.Case, bool, error) {
	var found []cases.Case
	var err error
	switch kind, _, _ := strings.Cut(identity, ":"); kind {
	case "finding":
		found, err = lookupFindingCases(ctx, mgr, sel, identity)
	case "incident":
		found, err = lookupIncidentCases(ctx, mgr, sel, identity)
	case "query_hint":
		found, err = lookupHintCases(ctx, sel, identity)
	}
	if err != nil {
		return cases.Case{}, false, err
	}
	cases.MarkExpiredCandidates(found, time.Now().UTC())
	for _, c := range found {
		if c.ID == identity {
			return c, true, nil
		}
	}
	return cases.Case{}, false, nil
}

// lookupFindingCases reads the open findings of one category, object type
// and object that the identity names.
func lookupFindingCases(
	ctx context.Context, mgr *fleet.DatabaseManager, sel namedPool, identity string,
) ([]cases.Case, error) {
	parts := strings.SplitN(identity, ":", 5)
	if len(parts) != 5 {
		return nil, nil
	}
	keys := findingSortKeys("severity")
	query := findingsColumns
	for _, k := range keys {
		query += ", " + k.expr
	}
	query += ` FROM sage.findings
 WHERE status = 'open' AND category = $1 AND COALESCE(object_type, '') = $2
   AND ($3 = ` + findingObjectKeySQL + ` OR left($3, length(` + findingObjectKeySQL +
		`) + 1) = ` + findingObjectKeySQL + ` || ':')
 ORDER BY last_seen DESC, id DESC LIMIT $4`
	rows, err := sel.pool.Query(ctx, query, parts[2], parts[3], parts[4], caseFocusLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paged, err := scanFindingPage(rows, sel.name, keys)
	if err != nil {
		return nil, err
	}
	maps := make([]map[string]any, len(paged))
	for i, p := range paged {
		maps[i] = p.row
	}
	attachSequenceRunway(ctx, sel.pool, sel.name, maps)
	out, findingIDs := projectFindingRows(maps)
	enrichCasesActionPolicies(ctx, out, mgr.GetInstance(sel.name))
	enrichFindingCases(ctx, out, sel, findingIDs)
	return out, nil
}

// lookupIncidentCases reads the open incident whose id ends the identity.
func lookupIncidentCases(
	ctx context.Context, mgr *fleet.DatabaseManager, sel namedPool, identity string,
) ([]cases.Case, error) {
	id := identity[strings.LastIndex(identity, ":")+1:]
	rows, err := sel.pool.Query(ctx, incidentsBaseSQL+
		` WHERE id::text = $1 AND resolved_at IS NULL LIMIT $2`, id, caseFocusLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scanned, err := scanIncidentRows(rows)
	if err != nil {
		return nil, err
	}
	out := projectIncidentRows(scanned, sel.name)
	enrichIncidentRecurrence(ctx, sel.pool, out)
	enrichCasesActionPolicies(ctx, out, mgr.GetInstance(sel.name))
	return out, nil
}

// lookupHintCases reads the query hints of the query the identity names.
func lookupHintCases(
	ctx context.Context, sel namedPool, identity string,
) ([]cases.Case, error) {
	parts := strings.SplitN(identity, ":", 4)
	if len(parts) != 4 {
		return nil, nil
	}
	queryID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, nil
	}
	rows, err := sel.pool.Query(ctx, queryHintsSQL+
		` WHERE queryid = $1 ORDER BY created_at DESC LIMIT $2`, queryID, caseFocusLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scanned, err := scanQueryHintRows(rows)
	if err != nil {
		return nil, err
	}
	return projectHintRows(ctx, sel, scanned), nil
}
