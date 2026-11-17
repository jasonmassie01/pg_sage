package inbox

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
)

// PolicyExceptions lists the policy-set exception in force on each
// registered database (fleetpol-01c): its own document instead of its
// set's. One without an expiry holds until someone replaces it, so it is
// moderate; one with an expiry is safe and due when the set's document
// applies again. An instance without a registry id has no exception.
func PolicyExceptions(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		inst, err := instanceOf(in, db)
		if err != nil {
			return nil, err
		}
		if inst.DatabaseID <= 0 {
			return []Item{}, nil
		}
		exc, err := policy.NewStore(inst.Pool).ExceptionOf(ctx, int64(inst.DatabaseID))
		if err != nil || exc == nil {
			return []Item{}, err
		}
		return []Item{exceptionItem(db, *exc)}, nil
	})
}

const exceptionFieldsShown = 2

func exceptionFields(diff []policy.FieldDiff) string {
	if len(diff) == 0 {
		return "same document as the set"
	}
	names := make([]string, 0, len(diff))
	for _, d := range diff {
		names = append(names, d.Field)
	}
	if len(names) <= exceptionFieldsShown {
		return strings.Join(names, ", ") + " differ"
	}
	return fmt.Sprintf("%s and %d more differ",
		strings.Join(names[:exceptionFieldsShown], ", "), len(names)-exceptionFieldsShown)
}

func exceptionItem(db string, e policy.Exception) Item {
	it := Item{Kind: KindPolicyException, ID: "exception-" + strconv.FormatInt(e.DatabaseID, 10),
		Database: db, CreatedAt: e.SetAt, Source: "sage.policy_exceptions",
		Title: fmt.Sprintf("Policy exception from set %s by %s: %s", e.SetName, e.SetBy,
			exceptionFields(e.Diff)),
		Why:  "it has no expiry and holds until replaced; set an expiry or fold it into the set",
		Risk: RiskModerate,
		Href: href("settings", "tab", "policy-sets", "database", db)}
	if e.ExpiresAt != nil {
		due := *e.ExpiresAt
		it.DueAt, it.Risk = &due, RiskSafe
		it.Why = "the set's document applies again when it expires on " +
			due.UTC().Format("2006-01-02")
	}
	return it
}

// joinSources lists every part under one kind. A part that fails fails
// the whole source, so the kind is reported unread and its total is a
// lower bound rather than silently short.
func joinSources(parts ...Source) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		out := []Item{}
		for _, p := range parts {
			got, err := p.List(ctx, db)
			if err != nil {
				return nil, err
			}
			out = append(out, got...)
		}
		return out, nil
	})
}
