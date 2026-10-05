package specialist

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Calibrator returns a family's bench calibration for a database, nil
// when there is none.
type Calibrator interface {
	Calibrate(ctx context.Context, database, family string) (*CalibratedRate, error)
}

type calibratorFunc func(ctx context.Context, database, family string) (*CalibratedRate,
	error)

func (f calibratorFunc) Calibrate(ctx context.Context, database,
	family string) (*CalibratedRate, error) {
	return f(ctx, database, family)
}

// Deps wires the service.
type Deps struct {
	Directory       Directory
	Store           RequestStore
	Calibrator      Calibrator // optional
	Limits          Limits
	KeepIdentifiers bool
	Now             func() time.Time
	Logger          *slog.Logger
}

// Service is the contract's logic, shared by the HTTP handler, the
// adapters and the MCP tools. Every call checks the caller's scope and
// database before any backend call.
type Service struct {
	dir             Directory
	store           RequestStore
	calibrator      Calibrator
	limits          Limits
	keepIdentifiers bool
	redactKey       []byte
	now             func() time.Time
	log             *slog.Logger
	writes, reads   *rateLimiter
	openMu          sync.Mutex // serializes the live-investigation bound
}

// NewService validates the wiring.
func NewService(d Deps) (*Service, error) {
	if d.Directory == nil || d.Store == nil {
		return nil, invalidf("the specialist service needs a directory and a store")
	}
	if err := d.Limits.Validate(); err != nil {
		return nil, err
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("specialist redaction key: %w", err)
	}
	return &Service{dir: d.Directory, store: d.Store, calibrator: d.Calibrator,
		limits: d.Limits, keepIdentifiers: d.KeepIdentifiers, redactKey: key, now: d.Now,
		log: d.Logger, writes: newRateLimiter(d.Limits.WritesPerMinute, d.Now),
		reads: newRateLimiter(d.Limits.ReadsPerMinute, d.Now)}, nil
}

var databaseName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$`)

// admit checks scope, database permission and the rate limit, in that
// order, and resolves the backend. Nothing reaches a backend before.
func (s *Service) admit(id Identity, scope, database string, write bool) (Backend, error) {
	if !id.Has(scope) {
		return nil, fmt.Errorf("%w: %s", ErrScope, scope)
	}
	if !databaseName.MatchString(database) {
		return nil, invalidf("invalid database name")
	}
	if !id.MayUse(database) {
		return nil, ErrDatabaseNotPermitted
	}
	limiter := s.reads
	if write {
		limiter = s.writes
	}
	if ok, wait := limiter.Allow(id.TokenID); !ok {
		return nil, &RateLimitError{RetryAfter: wait}
	}
	b, ok := s.dir.Backend(database)
	if !ok {
		return nil, fmt.Errorf("%w: database %q", ErrNotFound, database)
	}
	return b, nil
}

// backendErr maps investigator errors onto contract errors.
func backendErr(err error) error {
	var coded *codedError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &coded):
		return err
	case errors.Is(err, sre.ErrNotFound), errors.Is(err, sreaction.ErrProposalNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, sre.ErrInvalidRequest):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, sre.ErrMetadataUnavailable), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, sreaction.ErrHandoffBlocked):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return err
}

func parseInvestigationID(raw string) (sre.UUID, error) {
	id, err := sre.ParseUUID(raw)
	if err != nil {
		return "", invalidf("investigation id must be a UUID")
	}
	return id, nil
}

func links(database, id string) Links {
	p := BasePath + "/databases/" + url.PathEscape(database) + "/investigations/" + id
	return Links{Status: p, Stream: p + "/stream", Result: p + "/result"}
}

// Status is an investigation's progress.
func (s *Service) Status(ctx context.Context, id Identity, database,
	invID string) (StatusResponse, error) {
	b, err := s.admit(id, ScopeRead, database, false)
	if err != nil {
		return StatusResponse{}, err
	}
	uid, err := parseInvestigationID(invID)
	if err != nil {
		return StatusResponse{}, err
	}
	d, err := b.Detail(ctx, uid)
	if err != nil {
		return StatusResponse{}, backendErr(err)
	}
	return s.statusOf(database, d), nil
}

func (s *Service) statusOf(database string, d sre.Detail) StatusResponse {
	inv := d.Investigation
	m := mapper{red: sre.NewIdentifierRedactor(s.keepIdentifiers, s.redactKey)}
	st := StatusResponse{ContractVersion: ContractVersion, Database: database,
		Investigation: m.ref(inv), Phase: phaseOf(inv.State), ProbeCount: inv.ProbeCount,
		ModelTurns: inv.ModelTurns, PollAfterSeconds: 5, Links: links(database,
			string(inv.ID))}
	if inv.State.Terminal() {
		st.PollAfterSeconds = 0
	}
	return st
}

func phaseOf(st sre.State) string {
	switch {
	case st.Terminal():
		return "complete"
	case st == sre.StateCollecting || st == sre.StateNeedsEvidence:
		return "collecting_evidence"
	case st == sre.StateEvaluating:
		return "evaluating"
	case st == sre.StatePaused:
		return "paused"
	}
	return "queued"
}

// Result is the investigation result (outcome in_progress while it runs).
func (s *Service) Result(ctx context.Context, id Identity, database,
	invID string) (Result, error) {
	b, err := s.admit(id, ScopeRead, database, false)
	if err != nil {
		return Result{}, err
	}
	uid, err := parseInvestigationID(invID)
	if err != nil {
		return Result{}, err
	}
	return s.result(ctx, b, id.TokenID, database, uid)
}

// result loads and maps a result; tokenID selects the caller's own record.
func (s *Service) result(ctx context.Context, b Backend, tokenID, database string,
	uid sre.UUID) (Result, error) {
	d, err := b.Detail(ctx, uid)
	if err != nil {
		return Result{}, backendErr(err)
	}
	proposals, err := b.Proposals(ctx, uid)
	if errors.Is(err, ErrNoActions) {
		proposals, err = nil, nil
	}
	if err != nil {
		return Result{}, backendErr(err)
	}
	rec, err := s.store.ForInvestigation(ctx, tokenID, database, string(uid))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	d.Database = database
	return MapResult(Snapshot{Detail: d, Proposals: proposals, Record: rec}, MapOptions{
		KeepIdentifiers: s.keepIdentifiers, RedactKey: s.redactKey,
		Calibration: s.calibration(ctx, database, d), Now: s.now()}), nil
}

// calibration is advisory: a failure is logged and leaves the result
// uncalibrated, never invented.
func (s *Service) calibration(ctx context.Context, database string,
	d sre.Detail) *CalibratedRate {
	family := d.Investigation.Summary.Family
	if s.calibrator == nil || family == "" || d.Investigation.State != sre.StateConcluded {
		return nil
	}
	cal, err := s.calibrator.Calibrate(ctx, database, family)
	if err != nil {
		s.log.Warn("specialist: reading the bench calibration failed; the result stays "+
			"uncalibrated", "database", database, "family", family, "err", err)
		return nil
	}
	return cal
}
