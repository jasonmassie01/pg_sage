package sre

import "sync"

// OnceLog logs each notice key once. ModelNotices is shared by the whole
// process, so "the model turn is unavailable" is said once however many
// databases or investigations run without a model.
type OnceLog struct {
	mu   sync.Mutex
	seen map[string]bool
}

// ModelNotices is the process-wide once-log for model turn notices.
var ModelNotices = &OnceLog{}

// Log calls logFn the first time key is seen and reports whether it did.
func (o *OnceLog) Log(logFn func(level, msg string, args ...any), key, level, msg string,
	args ...any) bool {
	o.mu.Lock()
	first := !o.seen[key]
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[key] = true
	o.mu.Unlock()
	if first && logFn != nil {
		logFn(level, msg, args...)
	}
	return first
}

// Notice keys.
const (
	NoticeModelUnavailable = "model_unavailable"
	NoticeNoModelBudget    = "model_daily_budget_zero"
)

// NoteModelUnavailable says once (per OnceLog) that investigations run
// without the model turn, and why.
func NoteModelUnavailable(o *OnceLog, logFn func(level, msg string, args ...any),
	why string) {
	o.Log(logFn, NoticeModelUnavailable, "WARN", "sre: model turn unavailable (%s); "+
		"investigations run deterministically", why)
}
