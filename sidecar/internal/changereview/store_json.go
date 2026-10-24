package changereview

import (
	"encoding/json"
	"fmt"
)

// reviewJSON is a review's jsonb columns, canonical JSON each. A nil
// transcript or receipt is SQL NULL.
type reviewJSON struct {
	statements    []byte
	findings      []byte
	evidence      []byte
	dropped       []byte
	notDetermined []byte
	summary       []byte
	expectations  []byte
	transcript    []byte
	receipt       []byte
}

// jsonColumns encodes r's jsonb columns; nil slices are empty arrays.
func jsonColumns(r Review) (reviewJSON, error) {
	ev, err := canonicalEvidence(r.Evidence)
	if err != nil {
		return reviewJSON{}, err
	}
	var j reviewJSON
	parts := []struct {
		dst *[]byte
		v   any
	}{
		{&j.statements, orEmpty(r.Statements)}, {&j.findings, orEmpty(r.Findings)},
		{&j.evidence, orEmpty(ev)}, {&j.dropped, orEmpty(r.Dropped)},
		{&j.notDetermined, orEmpty(r.NotDetermined)}, {&j.summary, r.Summary},
		{&j.expectations, normalizeExpectations(r.Expectations)},
	}
	if r.Transcript != nil {
		parts = append(parts, struct {
			dst *[]byte
			v   any
		}{&j.transcript, r.Transcript})
	}
	if r.Receipt != nil {
		parts = append(parts, struct {
			dst *[]byte
			v   any
		}{&j.receipt, r.Receipt})
	}
	for _, p := range parts {
		canon, err := CanonicalJSON(p.v)
		if err != nil {
			return reviewJSON{}, fmt.Errorf("encode review column: %w", err)
		}
		*p.dst = canon
	}
	return j, nil
}

// orEmpty is s, or an empty slice of its type when nil, so it encodes as [].
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func normalizeExpectations(e Expectations) Expectations {
	e.PostConditions = orEmpty(e.PostConditions)
	e.WatchQueryIDs = orEmpty(e.WatchQueryIDs)
	e.Relations = orEmpty(e.Relations)
	e.Predictions = orEmpty(e.Predictions)
	return e
}

// decodeInto fills r's jsonb fields; evidence data is re-canonicalized
// (jsonb reorders keys and adds spaces).
func (j reviewJSON) decodeInto(r *Review) error {
	parts := []struct {
		name string
		raw  []byte
		dst  any
	}{
		{"statements", j.statements, &r.Statements}, {"findings", j.findings, &r.Findings},
		{"evidence", j.evidence, &r.Evidence}, {"dropped", j.dropped, &r.Dropped},
		{"not_determined", j.notDetermined, &r.NotDetermined},
		{"summary", j.summary, &r.Summary}, {"expectations", j.expectations, &r.Expectations},
		{"transcript", j.transcript, &r.Transcript}, {"receipt", j.receipt, &r.Receipt},
	}
	for _, p := range parts {
		if len(p.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(p.raw, p.dst); err != nil {
			return fmt.Errorf("decode %s: %w", p.name, err)
		}
	}
	ev, err := canonicalEvidence(r.Evidence)
	if err != nil {
		return err
	}
	r.Evidence = ev
	r.Statements, r.Findings = orEmpty(r.Statements), orEmpty(r.Findings)
	r.Evidence, r.Dropped = orEmpty(r.Evidence), orEmpty(r.Dropped)
	r.NotDetermined = orEmpty(r.NotDetermined)
	r.Expectations = normalizeExpectations(r.Expectations)
	return nil
}
