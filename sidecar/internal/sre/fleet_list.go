package sre

import (
	"context"
	"encoding/json"
)

// TaggedInvestigation is an investigation with its database's name, for
// lists that span databases.
type TaggedInvestigation struct {
	Database      string
	Investigation Investigation
}

// MarshalJSON is the investigation's JSON with a "database" field.
func (t TaggedInvestigation) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(t.Investigation)
	if err != nil {
		return nil, err
	}
	row := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	name, _ := json.Marshal(t.Database)
	row["database"] = name
	return json.Marshal(row)
}

// ListAcross lists the latest investigations of several databases. A
// database whose investigations cannot be read is named in unavailable
// instead of failing the whole list.
func ListAcross(ctx context.Context, services []*Service,
	f ListFilter) ([]TaggedInvestigation, []string, error) {
	items, unavailable := []TaggedInvestigation{}, []string{}
	for _, svc := range services {
		page, err := svc.List(ctx, f)
		switch {
		case ctx.Err() != nil:
			return nil, nil, ctx.Err()
		case err != nil:
			unavailable = append(unavailable, svc.Name())
			continue
		}
		for _, inv := range page.Items {
			items = append(items, TaggedInvestigation{Database: svc.Name(),
				Investigation: inv})
		}
	}
	return items, unavailable, nil
}
