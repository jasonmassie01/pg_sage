package whynot

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Chain checks the ten links in order and marks the first failure. Every
// link runs even when an earlier one fails or is unknown, so the answer
// lists every cause at once. The emergency stop is checked last but marked
// first when it fails: nothing runs while it is on. ctx is only checked:
// Chain reads nothing.
func Chain(ctx context.Context, in Inputs) Result {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	if err := ctx.Err(); err != nil && in.PolicyErr == nil {
		in.PolicyErr = err
	}
	links := []Link{
		observedLink(in), findingLink(in), candidateLink(in), policyLink(in),
		gateLink(in, LinkWindow, windowFact), gateLink(in, LinkBudget, budgetFact),
		gateLink(in, LinkPrivilege, privilegeFact), gateLink(in, LinkTrust, trustFact),
		gateLink(in, LinkVerification, nil), gateLink(in, LinkEmergencyStop, stopFact),
	}
	return markFirstFail(links)
}

// markFirstFail marks the stop when it fails, else the earliest failure.
func markFirstFail(links []Link) Result {
	res := Result{Links: links, Failing: []LinkName{}}
	first := -1
	for i, link := range links {
		if link.Status != Fail {
			continue
		}
		if first < 0 || link.Name == LinkEmergencyStop {
			first = i
		}
	}
	if first < 0 {
		return res
	}
	links[first].FirstFail = true
	res.FirstFail = links[first].Name
	res.Failing = append(res.Failing, links[first].Name)
	for i, link := range links {
		if link.Status == Fail && i != first {
			res.Failing = append(res.Failing, link.Name)
		}
	}
	return res
}

// ErrorClass names an error without its text, which can carry hosts and
// roles: a timeout, a cancellation, a permission error, or "error".
func ErrorClass(err error) string {
	var classed interface{ SQLState() string }
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &classed) && classed.SQLState() == "42501":
		return "permission_denied"
	case errors.As(err, &classed):
		return "sqlstate_" + classed.SQLState()
	}
	return "error"
}

func unknownLink(name LinkName, sentence string, err error) Link {
	return Link{Name: name, Status: Unknown, Sentence: sentence, Error: ErrorClass(err)}
}

// uiLink adds the database to a dashboard hash link.
func uiLink(page, database string) *Fix {
	if database == "" {
		return &Fix{Kind: FixUILink, Value: page}
	}
	sep := "?"
	if strings.Contains(page, "?") {
		sep = "&"
	}
	return &Fix{Kind: FixUILink, Value: page + sep + "database=" + url.QueryEscape(database)}
}

// waitTimes finds RFC 3339 times the gate writes into its details
// ("next frees at 2026-10-04T18:10:00Z", "(until ...)").
var waitTimes = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)`)

// waitFromDetail is a wait_until fix at the first time named in detail
// that is still ahead, else nil.
func waitFromDetail(detail string, now time.Time) *Fix {
	for _, raw := range waitTimes.FindAllString(detail, -1) {
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err == nil && at.After(now) {
			return &Fix{Kind: FixWaitUntil, Value: at.UTC().Format(time.RFC3339)}
		}
	}
	return nil
}

// resolveFix turns a mapping's fix into the served one.
func resolveFix(spec fixSpec, in Inputs, detail string) *Fix {
	var fix *Fix
	switch spec.kind {
	case "":
		return nil
	case fixCard:
		fix = cardLink(in)
	case fixWaitDetail:
		fix = waitFromDetail(detail, in.Now)
		if fix == nil {
			fix = uiLink(pageHeldBack, in.Database)
		}
	case FixUILink:
		fix = uiLink(spec.value, in.Database)
	default:
		fix = &Fix{Kind: spec.kind, Value: spec.value}
	}
	if fix.Note == "" {
		fix.Note = spec.note
	}
	return fix
}

// cardLink is the card of the finding asked about, else the approval
// queue of the database.
func cardLink(in Inputs) *Fix {
	if in.Findings.FindingID <= 0 {
		return uiLink(pagePending, in.Database)
	}
	return uiLink("#/cases?finding="+strconv.FormatInt(in.Findings.FindingID, 10),
		in.Database)
}
