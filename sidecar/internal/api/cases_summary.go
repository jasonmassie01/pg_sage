package api

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// caseListMaxLimit bounds ?limit= on GET /api/v1/cases.
const caseListMaxLimit = 100

// casesListOptions are the cases-11b list parameters: limit keeps the top
// N of the global rank (0 means every case) and summary drops each row's
// evidence, candidates and action history.
type casesListOptions struct {
	limit   int
	summary bool
}

// caseSummary is one ranked case row without its evidence payloads: what
// the Overview needs to name the case, place it in the rank and link to
// it (cases-11b).
type caseSummary struct {
	ID                  string           `json:"id"`
	SourceType          cases.SourceType `json:"source_type"`
	DatabaseName        string           `json:"database_name"`
	IdentityKey         string           `json:"identity_key"`
	Title               string           `json:"title"`
	Severity            cases.Severity   `json:"severity"`
	State               cases.State      `json:"state"`
	WhyNow              string           `json:"why_now"`
	ObservedAt          time.Time        `json:"observed_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
	Urgency             cases.Urgency    `json:"urgency"`
	RankBasis           string           `json:"rank_basis"`
	HasActionCandidates bool             `json:"has_action_candidates"`
}

func summarizeCase(c cases.Case) caseSummary {
	return caseSummary{
		ID: c.ID, SourceType: c.SourceType, DatabaseName: c.DatabaseName,
		IdentityKey: c.IdentityKey, Title: c.Title, Severity: c.Severity,
		State: c.State, WhyNow: c.WhyNow, ObservedAt: c.ObservedAt,
		UpdatedAt: c.UpdatedAt, Urgency: c.Urgency, RankBasis: c.RankBasis,
		HasActionCandidates: len(c.ActionCandidates) > 0,
	}
}

// parseCasesListOptions reads ?limit= (an integer from 1 to
// caseListMaxLimit) and ?summary= (1/true or 0/false). Neither combines
// with ?case=, whose focused case must come back whole.
func parseCasesListOptions(q url.Values) (casesListOptions, error) {
	var opts casesListOptions
	if q.Has("limit") {
		raw := q["limit"]
		n, err := strconv.Atoi(raw[0])
		if len(raw) != 1 || err != nil || n < 1 || n > caseListMaxLimit {
			return opts, fmt.Errorf(
				"invalid limit parameter: want one integer from 1 to %d", caseListMaxLimit)
		}
		opts.limit = n
	}
	if q.Has("summary") {
		switch raw := q["summary"]; {
		case len(raw) == 1 && (raw[0] == "1" || raw[0] == "true"):
			opts.summary = true
		case len(raw) == 1 && (raw[0] == "0" || raw[0] == "false"):
		default:
			return opts, errors.New("invalid summary parameter: want 1, true, 0 or false")
		}
	}
	if q.Get("case") != "" && (q.Has("limit") || q.Has("summary")) {
		return opts, errors.New(
			"invalid case parameter: case cannot be combined with limit or summary")
	}
	return opts, nil
}

// applyCasesListOptions cuts the ranked cases to the limit and, for a
// summary, replaces them with summary rows. total stays the count of every
// ranked case, so a caller can say "top 5 of N".
func applyCasesListOptions(body map[string]any, list []cases.Case, opts casesListOptions) {
	if opts.limit > 0 {
		body["limit"] = opts.limit
		if len(list) > opts.limit {
			list = list[:opts.limit]
		}
		body["cases"] = list
	}
	if !opts.summary {
		return
	}
	rows := make([]caseSummary, len(list))
	for i, c := range list {
		rows[i] = summarizeCase(c)
	}
	body["cases"], body["summary"] = rows, true
}
