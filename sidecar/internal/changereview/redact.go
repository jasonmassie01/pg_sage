package changereview

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// redact applies the configured redactor to one stored text.
func (s *Service) redact(text string) string {
	if s.d.Redact == nil || text == "" {
		return text
	}
	return s.d.Redact(text)
}

// redactReview scrubs every text the review stores: statements, findings
// and suggestions, what was dropped or not determined, evidence labels
// and every string inside evidence data (re-digested when it changed) and
// the transcript.
func (s *Service) redactReview(r *Review) error {
	if s.d.Redact == nil {
		return nil
	}
	for i := range r.Statements {
		st := &r.Statements[i]
		st.SQL, st.File = s.redact(st.SQL), s.redact(st.File)
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Title, f.Text = s.redact(f.Title), s.redact(f.Text)
		if f.Suggestion != nil {
			sug := *f.Suggestion
			sug.Code = s.redact(sug.Code)
			f.Suggestion = &sug
		}
	}
	for i := range r.Dropped {
		r.Dropped[i].Text = s.redact(r.Dropped[i].Text)
	}
	for i := range r.NotDetermined {
		n := &r.NotDetermined[i]
		n.Subject, n.Reason = s.redact(n.Subject), s.redact(n.Reason)
	}
	for i := range r.Evidence {
		if err := s.redactEvidence(&r.Evidence[i]); err != nil {
			return err
		}
	}
	if r.Transcript != nil {
		raw, err := json.Marshal(r.Transcript)
		if err != nil {
			return fmt.Errorf("encode the transcript: %w", err)
		}
		clean, err := s.redactJSON(raw)
		if err != nil {
			return err
		}
		r.Transcript = nil
		if err := json.Unmarshal(clean, &r.Transcript); err != nil {
			return fmt.Errorf("decode the redacted transcript: %w", err)
		}
	}
	return nil
}

func (s *Service) redactEvidence(e *Evidence) error {
	label := s.redact(e.Label)
	data, err := s.redactJSON(e.Data)
	if err != nil {
		return fmt.Errorf("evidence %s: %w", e.ID, err)
	}
	if label == e.Label && bytes.Equal(data, e.Data) {
		return nil
	}
	e.Label, e.Data = label, data
	e.Digest = evidenceDigest(*e)
	return nil
}

// redactJSON redacts every string leaf of a JSON document and returns it
// canonical. Keys are kept: they are pg_sage's own field names.
func (s *Service) redactJSON(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("decode for redaction: %w", err)
	}
	return CanonicalJSON(s.redactTree(tree))
}

func (s *Service) redactTree(v any) any {
	switch t := v.(type) {
	case string:
		return s.redact(t)
	case []any:
		for i := range t {
			t[i] = s.redactTree(t[i])
		}
		return t
	case map[string]any:
		for k, x := range t {
			t[k] = s.redactTree(x)
		}
		return t
	}
	return v
}

// redactMerged scrubs a merge result before anything of it is stored.
func (s *Service) redactMerged(m *Merged) error {
	r := Review{Statements: m.Statements, Findings: m.Findings, Evidence: m.Evidence,
		Dropped: m.Dropped, NotDetermined: m.NotDetermined, Transcript: m.Transcript}
	if err := s.redactReview(&r); err != nil {
		return fmt.Errorf("redact the review: %w", err)
	}
	m.Statements, m.Findings, m.Evidence = r.Statements, r.Findings, r.Evidence
	m.Dropped, m.NotDetermined, m.Transcript = r.Dropped, r.NotDetermined, r.Transcript
	return nil
}
