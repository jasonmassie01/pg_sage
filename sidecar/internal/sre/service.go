package sre

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Service is the read (and pin) side of one database's investigations,
// for the API, MCP tools and exports. Every call is scoped to the
// coordinator's bound database identity.
type Service struct {
	name  string
	coord *Coordinator
	store *PostgresStore
}

// NewService wraps a coordinator and its store; name is the database's
// display name.
func NewService(name string, coord *Coordinator, store *PostgresStore) *Service {
	return &Service{name: name, coord: coord, store: store}
}

// Coordinator is the investigation loop behind the service.
func (s *Service) Coordinator() *Coordinator { return s.coord }

// Name is the database's display name.
func (s *Service) Name() string { return s.name }

// scope is the bound database identity, binding now if needed.
func (s *Service) scope(ctx context.Context) (Scope, error) {
	if s == nil || s.coord == nil || s.store == nil {
		return Scope{}, fmt.Errorf("%w: investigations are not configured",
			ErrMetadataUnavailable)
	}
	return s.coord.Bind(ctx)
}

// List pages the database's investigations (redacted), newest first.
func (s *Service) List(ctx context.Context, f ListFilter) (Page, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Page{}, err
	}
	page, err := s.store.List(ctx, scope, f)
	if err != nil {
		return Page{}, err
	}
	out := Page{NextCursor: page.NextCursor, Items: []Investigation{}}
	for _, inv := range page.Items {
		var clean Investigation
		if err := redactInto(inv, &clean); err != nil {
			return Page{}, err
		}
		out.Items = append(out.Items, clean)
	}
	return out, nil
}

// EvidenceView is one evidence row as surfaces show it: redacted
// payload, hash and whether it still verifies.
type EvidenceView struct {
	ID              UUID            `json:"id"`
	Step            string          `json:"step"`
	ProbeID         string          `json:"probe_id"`
	ProbeVersion    string          `json:"probe_version"`
	CapabilityState string          `json:"capability_state"`
	ReasonCode      string          `json:"reason_code,omitempty"`
	ObservedAt      *time.Time      `json:"observed_at"`
	CollectedAt     time.Time       `json:"collected_at"`
	SHA256          string          `json:"sha256"`
	HashVerified    bool            `json:"hash_verified"`
	Payload         json.RawMessage `json:"payload"`
}

// Detail is one investigation with its latest diagnosis and evidence.
type Detail struct {
	Database          string             `json:"database"`
	Investigation     Investigation      `json:"investigation"`
	Hypotheses        []HypothesisRecord `json:"hypotheses"`
	Revisions         int                `json:"revisions"`
	Evidence          []EvidenceView     `json:"evidence"`
	EvidenceAvailable bool               `json:"evidence_available"`
	Tombstones        []Tombstone        `json:"tombstones"`
	EventCount        int                `json:"event_count"`
	ChainVerified     bool               `json:"chain_verified"`
	Degraded          bool               `json:"degraded"`
}

// Detail returns one investigation of this database with its latest
// diagnosis revision, redacted evidence and verification state.
func (s *Service) Detail(ctx context.Context, id UUID) (Detail, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Detail{}, err
	}
	b, err := s.load(ctx, scope, id)
	if err != nil {
		return Detail{}, err
	}
	latest, revisions := latestRevision(b.hypotheses)
	d := Detail{Database: s.name, Investigation: b.inv, Hypotheses: latest,
		Revisions: revisions, Evidence: b.evidence,
		EvidenceAvailable: b.inv.EvidencePurgedAt.IsZero(), Tombstones: b.tombstones,
		EventCount: len(b.events), ChainVerified: b.verified,
		Degraded: s.coord.Durability().Status().Degraded}
	var out Detail
	return out, redactInto(d, &out)
}

// bundle is everything stored about one investigation.
type bundle struct {
	inv        Investigation
	hypotheses []HypothesisRecord
	evidence   []EvidenceView
	tombstones []Tombstone
	events     []Event
	verified   bool
}

func (s *Service) load(ctx context.Context, scope Scope, id UUID) (bundle, error) {
	var b bundle
	var err error
	if err = validateIDs(scope, id); err != nil {
		return b, err
	}
	if b.inv, err = s.store.Get(ctx, scope, id); err != nil {
		return b, err
	}
	if b.hypotheses, err = s.store.Hypotheses(ctx, scope, id); err != nil {
		return b, err
	}
	ev, err := s.store.Evidence(ctx, scope, id)
	if err != nil {
		return b, err
	}
	b.evidence = views(ev)
	if b.tombstones, err = s.store.Tombstones(ctx, scope, id); err != nil {
		return b, err
	}
	b.events, b.verified, err = s.chain(ctx, scope, id)
	if b.tombstones == nil {
		b.tombstones = []Tombstone{}
	}
	return b, err
}

func (s *Service) chain(ctx context.Context, scope Scope, id UUID) ([]Event, bool, error) {
	events, err := s.store.Events(ctx, scope, id)
	if err != nil {
		return nil, false, err
	}
	err = s.store.VerifyEvents(ctx, scope, id)
	if err != nil && !errors.Is(err, ErrChainBroken) {
		return nil, false, err
	}
	return events, err == nil, nil
}

func latestRevision(hs []HypothesisRecord) ([]HypothesisRecord, int) {
	out := []HypothesisRecord{}
	revisions := 0
	for _, h := range hs {
		revisions = max(revisions, h.Revision)
	}
	for _, h := range hs {
		if h.Revision == revisions {
			out = append(out, h)
		}
	}
	return out, revisions
}

func views(ev []Evidence) []EvidenceView {
	out := make([]EvidenceView, 0, len(ev))
	for _, e := range ev {
		out = append(out, view(e))
	}
	return out
}

func view(e Evidence) EvidenceView {
	return EvidenceView{ID: e.ID, Step: e.StepKey, ProbeID: e.ProbeID,
		ProbeVersion: e.ProbeVersion, CapabilityState: e.CapabilityState,
		ReasonCode: e.ReasonCode, ObservedAt: timePtr(e.ObservedAt),
		CollectedAt: e.CollectedAt, SHA256: hex.EncodeToString(e.SHA256),
		HashVerified: e.VerifyHash(), Payload: json.RawMessage(e.Payload)}
}

// EvidenceItem returns one redacted evidence row of one investigation of
// this database; evidence of any other investigation is ErrNotFound.
func (s *Service) EvidenceItem(ctx context.Context, id, evidenceID UUID) (EvidenceView, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return EvidenceView{}, err
	}
	e, err := s.store.EvidenceByID(ctx, scope, id, evidenceID)
	if err != nil {
		return EvidenceView{}, err
	}
	var out EvidenceView
	return out, redactInto(view(e), &out)
}

// Events returns the investigation's redacted event chain and whether it
// verifies.
func (s *Service) Events(ctx context.Context, id UUID) ([]Event, bool, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := validateIDs(scope, id); err != nil {
		return nil, false, err
	}
	if _, err := s.store.Get(ctx, scope, id); err != nil {
		return nil, false, err
	}
	events, verified, err := s.chain(ctx, scope, id)
	if err != nil {
		return nil, false, err
	}
	var out []Event
	return out, verified, redactInto(events, &out)
}

// SetPinned pins (retention keeps it) or unpins an investigation for an
// actor.
func (s *Service) SetPinned(ctx context.Context, id UUID, pinned bool,
	actor string) (Investigation, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Investigation{}, err
	}
	inv, err := s.store.SetPinned(ctx, scope, id, pinned, actor)
	if err != nil {
		return Investigation{}, err
	}
	var clean Investigation
	return clean, redactInto(inv, &clean)
}
