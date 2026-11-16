package backtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// rngStream is the second word of the placebo RNG's seed ("lab-02").
const rngStream = 0x6c61622d3032

// window is the replayed span of history and the span placebos fall in.
type window struct {
	from, to         time.Time
	placeboFrom, end time.Time
}

// replayWindow is the last cfg.Days of the stored history. Less than
// MinHistoryDays of it is ErrNotEnoughHistory, saying how much there is.
func replayWindow(ctx context.Context, h History, cfg Config, now time.Time) (window,
	error) {
	first, last, ok, err := h.Span(ctx)
	if err != nil {
		return window{}, fmt.Errorf("read the extent of the stored history: %w", err)
	}
	if !ok {
		return window{}, fmt.Errorf("%w: 0 of %d days", ErrNotEnoughHistory, MinHistoryDays)
	}
	w := window{from: first.UTC(), to: last.UTC()}
	if now.Before(w.to) {
		w.to = now.UTC()
	}
	if start := w.to.Add(-time.Duration(cfg.Days) * day); start.After(w.from) {
		w.from = start
	}
	if days := int(w.to.Sub(w.from) / day); days < MinHistoryDays {
		return window{}, fmt.Errorf("%w: %d of %d days", ErrNotEnoughHistory, max(days, 0),
			MinHistoryDays)
	}
	w.placeboFrom = first.UTC().Add(SeasonalLead)
	if lo := w.from.Add(placeboCriterion.HardMax); lo.After(w.placeboFrom) {
		w.placeboFrom = lo
	}
	w.end = w.to.Add(-placeboCriterion.HardMax)
	return w, nil
}

// Run back-tests h: it draws cfg.Placebos placebos with cfg.Seed and
// replays each through v2 and v1, null and injected. It stops with
// ErrRuntimeExceeded (no result) past cfg.MaxRuntime, so a partial rate
// is never returned. The same seed on the same history gives the same
// counts.
func Run(ctx context.Context, h History, cfg Config) (Result, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Cycle <= 0 {
		cfg.Cycle = time.Minute
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	start := cfg.Now()
	deadline := start.Add(cfg.MaxRuntime)
	ctx, cancel := context.WithTimeout(ctx, cfg.MaxRuntime)
	defer cancel()
	w, err := replayWindow(ctx, h, cfg, start)
	if err != nil {
		return Result{}, err
	}
	placebos, err := drawFor(ctx, h, cfg, w)
	if err != nil {
		return Result{}, err
	}
	res := Result{Database: cfg.Database, RanAt: start.UTC(), From: w.from, To: w.to,
		Seed: cfg.Seed, Verifier: verify.VerifierV2}
	for i, p := range placebos {
		if cfg.Now().After(deadline) || ctx.Err() != nil {
			return Result{}, exceeded(cfg, i, len(placebos))
		}
		r, err := replay(ctx, h, p, cfg.Cycle)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				return Result{}, exceeded(cfg, i, len(placebos))
			}
			return Result{}, fmt.Errorf("placebo %d of %d at %s: %w", i+1, len(placebos),
				p.At.Format(time.RFC3339), err)
		}
		tally(&res.V2, r.excluded, r.v2, r.v2Injected)
		tally(&res.V1, r.excluded, r.v1, r.v1Injected)
		if p.Kind == verify.AttributionGlobal {
			res.Global++
		}
	}
	return res, nil
}

func exceeded(cfg Config, done, total int) error {
	return fmt.Errorf("%w (%s) after %d of %d placebos: no result stored",
		ErrRuntimeExceeded, cfg.MaxRuntime, done, total)
}

// drawFor reads the statements and actions and draws the placebos.
func drawFor(ctx context.Context, h History, cfg Config, w window) ([]Placebo, error) {
	stmts, err := h.Statements(ctx, w.to)
	if err != nil {
		return nil, fmt.Errorf("read the statements: %w", err)
	}
	actions, err := h.Actions(ctx, w.placeboFrom.Add(-ActionGap), w.end.Add(ActionGap))
	if err != nil {
		return nil, fmt.Errorf("read the executed actions: %w", err)
	}
	rng := rand.New(rand.NewPCG(uint64(cfg.Seed), rngStream))
	placebos := Draw(rng, stmts, actions, w.placeboFrom, w.end, cfg.Placebos)
	if len(placebos) == 0 {
		return nil, fmt.Errorf("no placebo could be drawn in %s..%s: %d statements, %d "+
			"executed actions", w.placeboFrom.Format(time.RFC3339),
			w.end.Format(time.RFC3339), len(stmts), len(actions))
	}
	return placebos, nil
}
