package modelpass

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// MaxRawOutputBytes caps a stored run's raw output (256 KiB).
const MaxRawOutputBytes = 256 << 10

// Run roles and statuses, as sage.review_model_run stores them.
const (
	RoleRank      = "rank"
	RoleRefute    = "refute"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// Skip reasons.
const (
	reasonTokenBudget = "token budget"
	reasonTurnedOff   = "review.model_pass turned off mid-review"
)

// runOutcome is one model run: its answer when done, else why not.
type runOutcome struct {
	attempt  int
	role     string
	status   string
	reason   string
	errClass string
	raw      string
	usage    usage
	res      agentloop.Result
}

type usage struct{ prompt, completion, reasoning int64 }

// recorder wraps the model: it keeps every reply as raw output, sums the
// reported tokens and remembers the last provider error. One per run.
type recorder struct {
	inner   agentloop.Model
	mu      sync.Mutex
	raw     strings.Builder
	usage   usage
	lastErr error
}

func (r *recorder) ChatWithTools(ctx context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	res, err := r.inner.ChatWithTools(ctx, msgs, tools, opts)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErr = err
		return res, err
	}
	r.usage.prompt += int64(res.PromptTokens)
	r.usage.completion += int64(res.CompletionTokens)
	r.usage.reasoning += int64(res.ReasoningTokens)
	if res.Content != "" {
		r.raw.WriteString(res.Content + "\n")
	}
	for _, tc := range res.ToolCalls {
		r.raw.WriteString(tc.Name + " " + string(tc.Arguments) + "\n")
	}
	return res, nil
}

// errClass is the last provider error's class (llm/errclass.go), or
// timeout for a step that ran out of time without one.
func (r *recorder) errClass() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := llm.ErrorClass(r.lastErr); c != "" {
		return c
	}
	if errors.Is(r.lastErr, context.DeadlineExceeded) {
		return "timeout"
	}
	return ""
}

// runOnce drives one agentloop run with cfg and classifies how it ended.
func runOnce(ctx context.Context, m agentloop.Model, attempt int, role string,
	cfg agentloop.Config) runOutcome {
	rec := &recorder{inner: m}
	res, err := agentloop.Run(ctx, rec, cfg)
	rec.mu.Lock()
	out := runOutcome{attempt: attempt, role: role, raw: rec.raw.String(), usage: rec.usage,
		res: res}
	rec.mu.Unlock()
	out.errClass = rec.errClass()
	stop := res.Transcript.Stop
	switch {
	case err != nil:
		out.status, out.reason = StatusFailed, "the run ended: "+err.Error()
	case stop == agentloop.StopBudget || stop == agentloop.StopTokens:
		out.status, out.reason = StatusSkipped, reasonTokenBudget
	case res.Final == nil:
		out.status = StatusFailed
		out.reason = strings.TrimSpace("the run stopped: " + stop + " " +
			res.Transcript.StopDetail)
	default:
		out.status = StatusDone
	}
	out.reason = clip(out.reason, 1000)
	return out
}

// capRaw cuts raw to MaxRawOutputBytes on a rune boundary; true when cut.
func capRaw(raw string) (string, bool) {
	if len(raw) <= MaxRawOutputBytes {
		return raw, false
	}
	cut := MaxRawOutputBytes
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return raw[:cut], true
}

// runMajority runs the N rank runs, at most Parallel at once. Before each
// run the live mode is read: once it is off, the runs not started yet are
// skipped and the ones in flight finish.
func (p *pass) runMajority(ctx context.Context) []runOutcome {
	n := p.s.Runs
	limit := p.d.Parallel
	if limit <= 0 || limit > n {
		limit = n
	}
	outs := make([]runOutcome, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		if p.turnedOff() {
			<-sem
			outs[i] = runOutcome{attempt: i + 1, role: RoleRank, status: StatusSkipped,
				reason: reasonTurnedOff}
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			outs[i] = runOnce(ctx, p.d.Model, i+1, RoleRank, p.rankConfig())
		}(i)
	}
	wg.Wait()
	return outs
}

func (p *pass) turnedOff() bool {
	return p.d.Mode != nil && p.d.Mode() == config.ReviewModelPassOff
}

// record stores every run once, with its final status. A failure stops at
// the first run it could not store.
func (p *pass) record(ctx context.Context, runs []runOutcome) error {
	if p.d.Store == nil {
		return nil
	}
	for _, o := range runs {
		if err := p.d.Store.RecordRun(ctx, p.recordOf(o)); err != nil {
			return fmt.Errorf("record model run %d of review run %d: %w", o.attempt,
				p.in.RunID, err)
		}
	}
	return nil
}

func (p *pass) recordOf(o runOutcome) RunRecord {
	sum := sha256.Sum256([]byte(o.raw))
	raw, cut := capRaw(o.raw)
	r := RunRecord{RunID: p.in.RunID, Attempt: o.attempt, Role: o.role, Model: p.d.ModelName,
		PromptVersion: PromptVersion, PackHash: p.in.Pack.ContentSHA256, RawOutput: raw,
		Truncated: cut, OutputSHA256: hex.EncodeToString(sum[:]), Status: o.status,
		ErrorClass: o.errClass, Reason: o.reason, TokensIn: o.usage.prompt,
		TokensOut: o.usage.completion, KeptClaims: p.kept[o.attempt],
		DroppedClaims: p.droppedOf[o.attempt]}
	if r.TokensIn+r.TokensOut > 0 {
		if cost, ok := llm.CostOf(p.d.Price, p.d.ModelName, o.usage.prompt,
			o.usage.completion, o.usage.reasoning); ok {
			r.CostUSD = &cost
		}
	}
	return r
}
