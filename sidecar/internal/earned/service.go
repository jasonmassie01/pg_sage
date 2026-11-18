package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
	// Build is the running pg_sage build: a bench report stamped for
	// another build is refused and never counts (roadmap 1.1).
	Build Build
	// AskAfterApprovals is how many person approvals of a self-initiated
	// class, with no rollback, make Evaluate ask for L3 (ux-07a,
	// trust.ask_after_approvals); 0 turns the ask off.
	AskAfterApprovals int
	Now               func() time.Time
	// Log reports failures that must not block a restriction (a cap
	// event that could not be written). Default: the standard logger.
	Log func(format string, args ...any)
}

// DefaultConfig is the conservative default.
func DefaultConfig() Config {
	return Config{Thresholds: DefaultThresholds(), ProposalTTL: 7 * 24 * time.Hour,
		MaxEvidenceAge: 5 * time.Minute, ConcurrencyWindow: 15 * time.Minute,
		SafetyWindow: 30 * 24 * time.Hour, FailoverCooldown: 30 * time.Minute,
		EvidenceCacheTTL: time.Minute, AskAfterApprovals: DefaultAskAfterApprovals,
		Now: time.Now}
}

// Service is one database's ledger: levels, proposals, approvals,
// downgrades, outcomes, reviews and evidence of that database within its
// deployment (P0-5), with the deployment's shared bench evidence.
type Service struct {
	store *PostgresStore
	cfg   Config

	mu    sync.Mutex
	cache map[pairKey]cachedLevel
	// invalidations counts the cache drops: an idle reconcile pass makes
	// none (earned-02).
	invalidations int64
	// ramp is the trust ramp, the promotion floor of self-initiated
	// classes (WithRamp); nil: unknown.
	ramp func() RampFloor
	// rootNotifier tells the operator about model-root authority changes
	// (WithRootAuthorityNotifier); nil: recorded only.
	rootNotifier RootAuthorityNotifier
	// modelOf is the model the database's investigator runs
	// (WithRunningModel); nil: unknown, so model roots stay advisory.
	modelOf func() string
}

type pairKey struct {
	family Family
	class  ActionClass
}

type cachedLevel struct {
	level Level
	ev    Evidence
	at    time.Time
}

// NewService builds the ledger over store.
func NewService(store *PostgresStore, cfg Config) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: no store", ErrUnavailable)
	}
	def := DefaultConfig()
	cfg.Thresholds = cfg.Thresholds.Normalized()
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
	cfg.AskAfterApprovals = max(cfg.AskAfterApprovals, 0)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = log.Printf
	}
	svc := &Service{store: store, cfg: cfg, cache: map[pairKey]cachedLevel{}}
	store.build = svc.Build
	return svc, nil
}

// Store is the ledger's store.
func (s *Service) Store() *PostgresStore { return s.store }

// Database is the database whose ledger this is.
func (s *Service) Database() string { return s.store.database }

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// defaultLevel: a shipped family starts at L1 (proposal only), anything
// else at L0.
func defaultLevel(f Family) Level {
	if KnownFamily(f) {
		return L1
	}
	return L0
}

// Granted is a pair's level (its default when unchanged): approved by a
// human, carried over, grandfathered or demoted.
func (s *Service) Granted(ctx context.Context, f Family, c ActionClass) (State, error) {
	st, found, err := s.store.readLevel(ctx, s.store.pool, f, c)
	if err != nil {
		return State{}, err
	}
	if !found {
		st = defaultState(f, c)
	}
	return s.boundByComponents(ctx, st, found)
}

// boundByComponents bounds a composite class's level by its components'
// granted levels (composedLevel).
func (s *Service) boundByComponents(ctx context.Context, st State, found bool) (State,
	error) {
	levels := map[Pair]Level{}
	for _, p := range ComponentClasses(st.Class) {
		cs, err := s.Granted(ctx, p.Family, p.Class)
		if err != nil {
			return State{}, err
		}
		levels[p] = cs.Level
	}
	return composedLevel(st, found, func(p Pair) Level { return levels[p] }), nil
}

// composedLevel bounds a composite class's state by its components'
// levels: without a row of its own (found) it takes their lowest level,
// with one the lower of its own and theirs. A plain class is unchanged.
func composedLevel(st State, found bool, componentLevel func(Pair) Level) State {
	comps := ComponentClasses(st.Class)
	if len(comps) == 0 {
		return st
	}
	lowest := MaxGrantable
	for _, p := range comps {
		lowest = MinLevel(lowest, componentLevel(p))
	}
	if found {
		lowest = MinLevel(st.Level, lowest)
	}
	st.Level = lowest
	return st
}

// defaultState is a pair without a stored row.
func defaultState(f Family, c ActionClass) State {
	return State{Family: f, Class: c, Level: defaultLevel(f), Provenance: ProvenanceLedger}
}

// supportedLevel is the level the pair's current evidence supports,
// cached for EvidenceCacheTTL.
func (s *Service) supportedLevel(ctx context.Context, f Family, c ActionClass) (Level,
	error) {
	level, _, err := s.supportedEvidence(ctx, f, c)
	return level, err
}

// supportedEvidence is the pair's current evidence and the level it
// supports, cached together for EvidenceCacheTTL.
func (s *Service) supportedEvidence(ctx context.Context, f Family, c ActionClass) (Level,
	Evidence, error) {
	key, now := pairKey{f, c}, s.now()
	if s.cfg.EvidenceCacheTTL > 0 {
		s.mu.Lock()
		hit, ok := s.cache[key]
		s.mu.Unlock()
		if ok && now.Sub(hit.at) < s.cfg.EvidenceCacheTTL {
			return hit.level, hit.ev, nil
		}
	}
	ev, err := s.Evidence(ctx, f, c)
	if err != nil {
		return L0, Evidence{}, err
	}
	level := SupportedLevel(s.cfg.Thresholds, ev)
	s.mu.Lock()
	s.cache[key] = cachedLevel{level: level, ev: ev, at: now}
	s.mu.Unlock()
	return level, ev, nil
}

// invalidate drops cached evidence (after outcomes, reviews, ingestion).
func (s *Service) invalidate() {
	s.mu.Lock()
	s.cache = map[pairKey]cachedLevel{}
	s.invalidations++
	s.mu.Unlock()
}

// ProposePromotions proposes what the evidence supports (Evaluate) and
// returns the proposals it created.
func (s *Service) ProposePromotions(ctx context.Context) ([]Proposal, error) {
	e, err := s.Evaluate(ctx)
	return e.Created, err
}

// createProposal stores a pending proposal of st to target on ev, held
// as a; kind names what it rests on ("" the bar, or approval_history).
func (s *Service) createProposal(ctx context.Context, st State, target Level, ev Evidence,
	a Assessment, kind string) (Proposal, bool, error) {
	raw, err := json.Marshal(struct {
		Kind       string     `json:"kind,omitempty"`
		Evidence   Evidence   `json:"evidence"`
		Assessment Assessment `json:"assessment"`
	}{kind, ev, a})
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
		Actor: ActorPgSage, Reason: proposalReasonOf(p.To, ev, kind),
		ProposalID: p.ID, Evidence: raw, At: now})
	if err != nil {
		return Proposal{}, false, err
	}
	stored, err := s.store.Proposal(ctx, p.ID)
	return stored, err == nil, err
}

// proposalReasonOf is proposalReason, or the approval history a kind
// approval_history proposal rests on.
func proposalReasonOf(to Level, ev Evidence, kind string) string {
	h := ev.ApprovalHistory
	if kind != EvidenceKindApprovalHistory || h == nil {
		return proposalReason(to, ev.Bench)
	}
	return fmt.Sprintf("approval history supports %s: %d person approvals of %s with no "+
		"rollback in 30 days (ask after %d)", to, h.Approvals, ev.Class, h.Required)
}

// proposalReason says what a proposal rests on: the level the evidence
// supports and, with bench evidence, the deciding report's provenance and
// id. The newest report decides whatever its origin, so the approver must
// see which one it is.
func proposalReason(to Level, bench *EvalRun) string {
	reason := fmt.Sprintf("evidence supports %s", to)
	if bench == nil {
		return reason
	}
	return strings.TrimSpace(fmt.Sprintf("%s; bench: %s %s", reason,
		bench.ProvenanceLabel(), bench.ID))
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
