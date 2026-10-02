package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Config tunes the ledger. Zero durations take the defaults.
type Config struct {
	Thresholds Thresholds
	// ProposalTTL is how long a promotion proposal waits for a human.
	ProposalTTL time.Duration
	// MaxEvidenceAge is the oldest action evidence that keeps L2/L3.
	MaxEvidenceAge time.Duration
	// ConcurrencyWindow is how recent another pg_sage action on the same
	// object must be to count as concurrent.
	ConcurrencyWindow time.Duration
	// SafetyWindow is how long a family safety regression caps the family.
	SafetyWindow time.Duration
	// FailoverCooldown is how long after a role change autonomy stays down.
	FailoverCooldown time.Duration
	// EvidenceCacheTTL caches promotion evidence per pair for the gate;
	// 0 reads it on every authorization.
	EvidenceCacheTTL time.Duration
	Now              func() time.Time
	// Log reports failures that must not block a restriction (a cap
	// event that could not be written). Default: the standard logger.
	Log func(format string, args ...any)
}

// DefaultConfig is the conservative default.
func DefaultConfig() Config {
	return Config{Thresholds: DefaultThresholds(), ProposalTTL: 7 * 24 * time.Hour,
		MaxEvidenceAge: 5 * time.Minute, ConcurrencyWindow: 15 * time.Minute,
		SafetyWindow: 30 * 24 * time.Hour, FailoverCooldown: 30 * time.Minute,
		EvidenceCacheTTL: time.Minute, Now: time.Now}
}

// Service is the ledger: levels, proposals, approvals, downgrades,
// outcomes, reviews and evidence of one deployment.
type Service struct {
	store *PostgresStore
	cfg   Config

	mu    sync.Mutex
	cache map[pairKey]cachedLevel
}

type pairKey struct {
	family Family
	class  ActionClass
}

type cachedLevel struct {
	level Level
	at    time.Time
}

// NewService builds the ledger over store.
func NewService(store *PostgresStore, cfg Config) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: no store", ErrUnavailable)
	}
	def := DefaultConfig()
	if cfg.Thresholds == (Thresholds{}) {
		cfg.Thresholds = def.Thresholds
	}
	for _, d := range []struct{ got, def *time.Duration }{
		{&cfg.ProposalTTL, &def.ProposalTTL}, {&cfg.MaxEvidenceAge, &def.MaxEvidenceAge},
		{&cfg.ConcurrencyWindow, &def.ConcurrencyWindow},
		{&cfg.SafetyWindow, &def.SafetyWindow}, {&cfg.FailoverCooldown, &def.FailoverCooldown},
	} {
		if *d.got <= 0 {
			*d.got = *d.def
		}
	}
	if cfg.EvidenceCacheTTL < 0 {
		cfg.EvidenceCacheTTL = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = log.Printf
	}
	return &Service{store: store, cfg: cfg, cache: map[pairKey]cachedLevel{}}, nil
}

// Store is the ledger's store.
func (s *Service) Store() *PostgresStore { return s.store }

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// defaultLevel: a shipped family starts at L1 (proposal only), anything
// else at L0.
func defaultLevel(f Family) Level {
	if KnownFamily(f) {
		return L1
	}
	return L0
}

// Granted is a pair's human-approved level (its default when unchanged).
func (s *Service) Granted(ctx context.Context, f Family, c ActionClass) (State, error) {
	st, found, err := s.store.readLevel(ctx, s.store.pool, f, c)
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{Family: f, Class: c, Level: defaultLevel(f)}, nil
	}
	return st, nil
}

// Evidence collects everything behind a promotion of the pair now.
func (s *Service) Evidence(ctx context.Context, f Family, c ActionClass) (Evidence, error) {
	now := s.now()
	ev := Evidence{Family: f, Class: c, At: now}
	var err error
	if ev.Bench, err = s.store.LatestBench(ctx, f); err != nil {
		return Evidence{}, err
	}
	since := now.Add(-s.cfg.Thresholds.BenchMaxAge)
	if ev.GameDays, err = s.store.GameDayRuns(ctx, since); err != nil {
		return Evidence{}, err
	}
	shadowSince := now.Add(-s.cfg.Thresholds.ShadowDuration)
	if ev.Shadow, err = s.store.ShadowStats(ctx, f, shadowSince); err != nil {
		return Evidence{}, err
	}
	if ev.Live, err = s.store.LiveStats(ctx, f, c); err != nil {
		return Evidence{}, err
	}
	ev.FamilyViolations, err = s.store.FamilyViolations(ctx, f, now.Add(-s.cfg.SafetyWindow))
	if err != nil {
		return Evidence{}, err
	}
	return ev, nil
}

// supportedLevel is the level the pair's current evidence supports,
// cached for EvidenceCacheTTL.
func (s *Service) supportedLevel(ctx context.Context, f Family, c ActionClass) (Level,
	error) {
	key, now := pairKey{f, c}, s.now()
	if s.cfg.EvidenceCacheTTL > 0 {
		s.mu.Lock()
		hit, ok := s.cache[key]
		s.mu.Unlock()
		if ok && now.Sub(hit.at) < s.cfg.EvidenceCacheTTL {
			return hit.level, nil
		}
	}
	ev, err := s.Evidence(ctx, f, c)
	if err != nil {
		return L0, err
	}
	level := SupportedLevel(s.cfg.Thresholds, ev)
	s.mu.Lock()
	s.cache[key] = cachedLevel{level: level, at: now}
	s.mu.Unlock()
	return level, nil
}

// invalidate drops cached evidence (after outcomes, reviews, ingestion).
func (s *Service) invalidate() {
	s.mu.Lock()
	s.cache = map[pairKey]cachedLevel{}
	s.mu.Unlock()
}

// ProposePromotions expires stale proposals, then proposes one level up
// for every applicable pair whose evidence supports it, below its cap and
// without a pending proposal. It never changes a level.
func (s *Service) ProposePromotions(ctx context.Context) ([]Proposal, error) {
	if err := s.expire(ctx); err != nil {
		return nil, err
	}
	created := []Proposal{}
	for _, f := range Families() {
		for _, c := range ApplicableClasses(f) {
			p, ok, err := s.proposeOne(ctx, f, c)
			if err != nil {
				return created, err
			}
			if ok {
				created = append(created, p)
			}
		}
	}
	return created, nil
}

func (s *Service) proposeOne(ctx context.Context, f Family, c ActionClass) (Proposal, bool,
	error) {
	st, err := s.Granted(ctx, f, c)
	if err != nil {
		return Proposal{}, false, err
	}
	target := st.Level + 1
	if target > CapFor(c) || !target.Grantable() {
		return Proposal{}, false, nil
	}
	ev, err := s.Evidence(ctx, f, c)
	if err != nil {
		return Proposal{}, false, err
	}
	a := Assess(s.cfg.Thresholds, target, ev)
	if !a.Met {
		return Proposal{}, false, nil
	}
	return s.createProposal(ctx, st, target, ev, a)
}

func (s *Service) createProposal(ctx context.Context, st State, target Level, ev Evidence,
	a Assessment) (Proposal, bool, error) {
	raw, err := json.Marshal(struct {
		Evidence   Evidence   `json:"evidence"`
		Assessment Assessment `json:"assessment"`
	}{ev, a})
	if err != nil {
		return Proposal{}, false, fmt.Errorf("%w: encode evidence: %v", ErrInvalidRequest, err)
	}
	now := s.now()
	p := Proposal{ID: newID(), Family: st.Family, Class: st.Class, From: st.Level,
		To: target, Evidence: raw, Status: StatusPending, ProposedAt: now,
		ExpiresAt: now.Add(s.cfg.ProposalTTL)}
	inserted, err := s.store.insertProposal(ctx, s.store.pool, p)
	if err != nil || !inserted {
		return Proposal{}, false, err
	}
	err = s.store.appendEvent(ctx, s.store.pool, Event{Family: p.Family, Class: p.Class,
		Type: EventPromotionProposed, From: levelPtr(p.From), To: levelPtr(p.To),
		Actor: ActorPgSage, Reason: fmt.Sprintf("evidence supports %s", p.To),
		ProposalID: p.ID, Evidence: raw, At: now})
	if err != nil {
		return Proposal{}, false, err
	}
	stored, err := s.store.Proposal(ctx, p.ID)
	return stored, err == nil, err
}

// expire marks pending proposals past their expiry and records it.
func (s *Service) expire(ctx context.Context) error {
	now := s.now()
	expired, err := s.store.expireDue(ctx, now)
	if err != nil {
		return err
	}
	for _, p := range expired {
		if err := s.store.appendEvent(ctx, s.store.pool, Event{Family: p.Family,
			Class: p.Class, Type: EventPromotionExpired, From: levelPtr(p.From),
			To: levelPtr(p.To), Actor: ActorPgSage, Reason: "no decision before expiry",
			ProposalID: p.ID, At: now}); err != nil {
			return err
		}
	}
	return nil
}

// PendingProposals lists proposals waiting for a human.
func (s *Service) PendingProposals(ctx context.Context) ([]Proposal, error) {
	if err := s.expire(ctx); err != nil {
		return nil, err
	}
	return s.store.listProposals(ctx, StatusPending, 200)
}

// Proposal reads one proposal.
func (s *Service) Proposal(ctx context.Context, id string) (Proposal, error) {
	return s.store.Proposal(ctx, id)
}

// History reads the ledger history, newest first.
func (s *Service) History(ctx context.Context, f EventFilter) ([]Event, error) {
	return s.store.Events(ctx, f)
}

func (c Config) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}
