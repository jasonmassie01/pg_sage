package specialist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// externalSubject is the subject of every contract-opened investigation.
// The caller's text never becomes a subject (it would reach probes'
// diagnosis and model prompts); one subject also lets concurrent callers
// of one family share one live investigation.
const externalSubject = "external request"

// windowSlack widens a caller's window when attaching to investigations.
const windowSlack = 5 * time.Minute

// incidentSearchPages bounds the attach-by-incident search (50 per page).
const incidentSearchPages = 5

// Open opens an investigation of database, or attaches to an existing
// one (by investigation id, incident id, or the same family in the
// caller's window). Opening needs the read scope: an investigation only
// reads catalog and statistics views.
func (s *Service) Open(ctx context.Context, id Identity, database string,
	req OpenRequest) (OpenResponse, error) {
	if err := req.Validate(s.now()); err != nil {
		return OpenResponse{}, err
	}
	b, err := s.admit(id, ScopeRead, database, true)
	if err != nil {
		return OpenResponse{}, err
	}
	inv, match, err := s.find(ctx, b, req)
	if err != nil {
		return OpenResponse{}, err
	}
	created := false
	if inv == nil {
		var started sre.Investigation
		started, created, err = s.openNew(ctx, id, b, database, req)
		if err != nil {
			return OpenResponse{}, err
		}
		inv, match = &started, "new"
		if !created {
			match = "idempotent"
		}
	} else if _, err := s.record(ctx, id, database, req, *inv, false, match); err != nil {
		return OpenResponse{}, err
	}
	m := mapper{red: sre.NewIdentifierRedactor(s.keepIdentifiers, s.redactKey)}
	return OpenResponse{ContractVersion: ContractVersion, Database: database,
		Investigation: m.ref(*inv), Created: created, Match: match,
		Links: links(database, string(inv.ID))}, nil
}

// find resolves an attach request or a same-family investigation in the
// caller's window; nil when a new one is to be opened.
func (s *Service) find(ctx context.Context, b Backend, req OpenRequest) (*sre.Investigation,
	string, error) {
	if a := req.Attach; a != nil {
		if a.InvestigationID != "" {
			d, err := b.Detail(ctx, sre.UUID(a.InvestigationID))
			if err != nil {
				return nil, "", backendErr(err)
			}
			return &d.Investigation, "investigation_id", nil
		}
		inv, err := s.byIncident(ctx, b, a.IncidentID)
		return inv, "incident_id", err
	}
	if req.Window == nil {
		return nil, "", nil
	}
	inv, err := s.inWindow(ctx, b, req)
	return inv, "window", err
}

func (s *Service) byIncident(ctx context.Context, b Backend,
	incident string) (*sre.Investigation, error) {
	f := sre.ListFilter{Limit: 50}
	for page := 0; page < incidentSearchPages; page++ {
		p, err := b.List(ctx, f)
		if err != nil {
			return nil, backendErr(err)
		}
		for i := range p.Items {
			if p.Items[i].IncidentID == incident {
				return &p.Items[i], nil
			}
		}
		if p.NextCursor == "" {
			break
		}
		f.Cursor = p.NextCursor
	}
	return nil, fmt.Errorf("%w: no investigation of incident %q", ErrNotFound, incident)
}

// inWindow returns the newest investigation of the requested family that
// started inside the caller's window (widened by windowSlack), live ones
// first; nil when there is none.
func (s *Service) inWindow(ctx context.Context, b Backend,
	req OpenRequest) (*sre.Investigation, error) {
	p, err := b.List(ctx, sre.ListFilter{Limit: 50})
	if err != nil {
		return nil, backendErr(err)
	}
	start, end := req.Window.Start.Add(-windowSlack), s.now().Add(windowSlack)
	if req.Window.End != nil {
		end = req.Window.End.Add(windowSlack)
	}
	kind := triggerFor(req.Family)
	var best *sre.Investigation
	for i := range p.Items {
		inv := &p.Items[i]
		if inv.TriggerKind != kind || inv.CreatedAt.Before(start) || inv.CreatedAt.After(end) {
			continue
		}
		if best == nil || (inv.State.Live() && !best.State.Live()) {
			best = inv
		}
	}
	return best, nil
}

// openNew starts an investigation under the live-investigation bounds.
// The bound check, the start and the record are serialized so parallel
// opens cannot overshoot.
func (s *Service) openNew(ctx context.Context, id Identity, b Backend, database string,
	req OpenRequest) (sre.Investigation, bool, error) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if err := s.checkBounds(ctx, id); err != nil {
		return sre.Investigation{}, false, err
	}
	inv, created, err := b.Start(ctx, sre.Trigger{CaseID: "specialist:" +
		string(sre.NewUUID()), Kind: triggerFor(req.Family), Subject: externalSubject,
		IdempotencyKey: idempotencyKey(id, req), Actor: id.Actor()})
	if err != nil {
		return sre.Investigation{}, false, backendErr(err)
	}
	if _, err := s.record(ctx, id, database, req, inv, created, ""); err != nil {
		return sre.Investigation{}, false, err
	}
	return inv, created, nil
}

// idempotencyKey scopes the caller's key (or its external reference) to
// its identity and bounds it; "" lets the investigator coalesce by trigger.
func idempotencyKey(id Identity, req OpenRequest) string {
	key := req.IdempotencyKey
	if key == "" && req.ExternalRef != nil {
		key = "ext:" + req.ExternalRef.System + ":" + req.ExternalRef.ID
	}
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id.TokenID + "\x00" + key))
	return "spec:" + hex.EncodeToString(sum[:16])
}

func (s *Service) checkBounds(ctx context.Context, id Identity) error {
	mine, err := s.liveCount(ctx, id.TokenID)
	if err != nil {
		return err
	}
	if mine >= s.limits.MaxOpenPerIdentity {
		return fmt.Errorf("%w: %d live investigations opened by this token (limit %d)",
			ErrTooManyInvestigations, mine, s.limits.MaxOpenPerIdentity)
	}
	all, err := s.liveCount(ctx, "")
	if err != nil {
		return err
	}
	if all >= s.limits.MaxOpenTotal {
		return fmt.Errorf("%w: %d live investigations opened through the contract "+
			"(limit %d)", ErrTooManyInvestigations, all, s.limits.MaxOpenTotal)
	}
	return nil
}

// liveCount counts the still-live investigations tokenID opened, marking
// finished ones terminal. An investigation that cannot be read counts as
// live (the bound fails closed).
func (s *Service) liveCount(ctx context.Context, tokenID string) (int, error) {
	refs, err := s.store.LiveOpened(ctx, tokenID)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	live := 0
	for _, ref := range refs {
		if s.stillLive(ctx, ref) {
			live++
			continue
		}
		if err := s.store.MarkTerminal(ctx, ref.RecordID); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	return live, nil
}

func (s *Service) stillLive(ctx context.Context, ref LiveRef) bool {
	b, ok := s.dir.Backend(ref.Database)
	if !ok {
		return false
	}
	d, err := b.Detail(ctx, sre.UUID(ref.InvestigationID))
	if err != nil {
		return !isNotFound(backendErr(err))
	}
	return !d.Investigation.State.Terminal()
}

// record audits an open or attach; adapter opens owe their system a result
// post unless one is already queued for the same external reference.
func (s *Service) record(ctx context.Context, id Identity, database string,
	req OpenRequest, inv sre.Investigation, created bool, match string) (Record, error) {
	r := Record{Kind: KindAttach, TokenID: id.TokenID, IdentityName: id.Name,
		Actor: id.Actor(), Transport: transportOf(id), Database: database,
		InvestigationID: string(inv.ID), Created: created, Match: match,
		Symptom: scrubSymptom(req.Symptom), Window: req.Window,
		ExternalRef: req.ExternalRef, Outbound: OutboundNone}
	if match == "" {
		r.Kind, r.Match = KindOpen, "new"
		if !created {
			r.Match = "idempotent"
		}
	}
	if r.Transport == "pagerduty" || r.Transport == "webhook" {
		owed, err := s.outboundOwed(ctx, req.ExternalRef)
		if err != nil {
			return Record{}, err
		}
		if owed {
			r.Outbound = OutboundPending
		}
	}
	out, err := s.store.Record(ctx, r)
	if err != nil {
		return Record{}, fmt.Errorf("%w: auditing the request: %v", ErrUnavailable, err)
	}
	return out, nil
}

func (s *Service) outboundOwed(ctx context.Context, ref *ExternalRef) (bool, error) {
	if ref == nil {
		return true, nil
	}
	queued, err := s.store.HasExternal(ctx, ref.System, ref.ID)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return !queued, nil
}

func transportOf(id Identity) string {
	switch id.Transport {
	case "mcp", "pagerduty", "webhook":
		return id.Transport
	}
	return "http"
}

func isNotFound(err error) bool { return codeOf(err) == ErrNotFound }

// scrubSymptom removes secrets and PII before the caller's text is stored.
func scrubSymptom(s *Symptom) *Symptom {
	if s == nil {
		return nil
	}
	return &Symptom{Summary: sre.Scrub(s.Summary), Description: sre.Scrub(s.Description)}
}
