package facts

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// Worker defaults.
const (
	defaultInterval      = 15 * time.Minute
	defaultModelInterval = 24 * time.Hour
)

// WorkerOptions configure the facts worker of one database.
type WorkerOptions struct {
	Detectors []Detector
	// Model and ModelEvidence, when both set, ask the model for proposals
	// at most once per ModelInterval.
	Model         *ModelProposer
	ModelEvidence func(context.Context) ([]EvidenceItem, error)
	Interval      time.Duration
	ModelInterval time.Duration
	// New and reopened proposals reach the pool's proposal notifier
	// (SetProposalNotifier), which sends their fact cards.
	// NotifyExpired is told about every confirmed fact re-verification
	// expired: a person's decision that no longer binds (facts-08).
	NotifyExpired func(context.Context, Fact)
	LogFn         func(string, string, ...any)
	Now           func() time.Time
}

// Worker imports declared contracts, runs the detectors (and the model),
// records their proposals and re-verifies facts.
type Worker struct {
	store     *Store
	opts      WorkerOptions
	lastModel time.Time
}

// RunStats is what one pass did.
type RunStats struct {
	Imported, Proposed, Expired int
	Errors                      []string
}

// NewWorker returns the worker of store's database.
func NewWorker(store *Store, opts WorkerOptions) *Worker {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.ModelInterval <= 0 {
		opts.ModelInterval = defaultModelInterval
	}
	if opts.LogFn == nil {
		opts.LogFn = func(string, string, ...any) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Worker{store: store, opts: opts}
}

// Run passes every Interval until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	for {
		w.log(w.RunOnce(ctx))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) log(s RunStats) {
	for _, e := range s.Errors {
		w.opts.LogFn("WARN", "facts: %s", e)
	}
	if s.Imported+s.Proposed+s.Expired > 0 {
		w.opts.LogFn("INFO", "facts: %d imported, %d proposed, %d expired",
			s.Imported, s.Proposed, s.Expired)
	}
}

// RunOnce is one pass. A failing detector or model is recorded and the
// pass goes on.
func (w *Worker) RunOnce(ctx context.Context) RunStats {
	ctx, done := selfbudget.Process().TrackFor(ctx, "facts")
	defer done()
	var stats RunStats
	fail := func(format string, args ...any) {
		stats.Errors = append(stats.Errors, fmt.Sprintf(format, args...))
	}
	n, err := w.store.ImportDeclared(ctx)
	if err != nil {
		fail("import declared contracts: %v", err)
	}
	stats.Imported = n
	fresh := map[int64]bool{}
	for _, d := range w.opts.Detectors {
		props, err := d.Detect(ctx)
		if err != nil {
			fail("detector %s: %v", d.Name(), err)
			continue
		}
		w.proposeAll(ctx, props, fresh, &stats)
	}
	w.askModel(ctx, fresh, &stats, fail)
	w.reverify(ctx, fresh, &stats, fail)
	return stats
}

// reverify re-checks the facts; every confirmed fact it expires is logged
// and notified, since it stops binding without anyone deciding so.
func (w *Worker) reverify(ctx context.Context, fresh map[int64]bool, stats *RunStats,
	fail func(string, ...any)) {
	ids := make([]int64, 0, len(fresh))
	for id := range fresh {
		ids = append(ids, id)
	}
	expired, err := w.store.Reverify(ctx, w.opts.Now(), ids...)
	if err != nil {
		fail("re-verify facts: %v", err)
	}
	stats.Expired = len(expired)
	for _, f := range expired {
		if !f.ConfirmationLost() {
			continue
		}
		w.opts.LogFn("WARN", "facts: confirmed fact #%d (%s, confirmed by %s) expired and "+
			"no longer binds: %s", f.ID, f.Describe(), f.DecidedBy, f.ExpiredReason)
		if w.opts.NotifyExpired != nil {
			w.opts.NotifyExpired(ctx, f)
		}
	}
}

func (w *Worker) askModel(ctx context.Context, fresh map[int64]bool, stats *RunStats,
	fail func(string, ...any)) {
	if w.opts.Model == nil || w.opts.ModelEvidence == nil {
		return
	}
	now := w.opts.Now()
	if !w.lastModel.IsZero() && now.Sub(w.lastModel) < w.opts.ModelInterval {
		return
	}
	w.lastModel = now
	evidence, err := w.opts.ModelEvidence(ctx)
	if err != nil {
		fail("model evidence: %v", err)
		return
	}
	props, err := w.opts.Model.Propose(ctx, evidence)
	if err != nil {
		fail("model proposals: %v", err)
		return
	}
	w.proposeAll(ctx, props, fresh, stats)
}

// proposeAll records proposals; the facts they touch count as freshly
// observed for this pass's re-verification.
func (w *Worker) proposeAll(ctx context.Context, props []Proposal, fresh map[int64]bool,
	stats *RunStats) {
	for _, p := range props {
		f, created, err := w.store.Propose(ctx, p)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("propose %s %q: %v", p.Type,
				p.Subject, err))
			continue
		}
		fresh[f.ID] = true
		if created {
			stats.Proposed++
		}
	}
}
