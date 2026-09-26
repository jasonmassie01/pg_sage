package providerobs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

const (
	// logOverlap re-reads the last minute to absorb late provider delivery.
	logOverlap = time.Minute
	// maxLogWindow caps one request so catch-up after an outage proceeds
	// in bounded steps instead of one ever-growing window (G1-B10).
	maxLogWindow = 10 * time.Minute
	// minLogWindow is the smallest window that still advances the cursor
	// past the overlap.
	minLogWindow = 2 * time.Minute
	// maxLogLag bounds how far the cursor may fall behind. Beyond it the
	// cursor skips ahead and reports the gap; the provider rejects
	// windows over 24h, so an unbounded lag used to wedge ingestion.
	maxLogLag = 6 * time.Hour
)

func (r *Runtime) pollLogs(ctx context.Context, now time.Time) error {
	if r.handler == nil {
		return nil
	}
	// Leave a minute for provider delivery before reading.
	until := now.UTC().Truncate(time.Minute).Add(-time.Minute)
	from := r.through.Add(-logOverlap)
	if r.through.IsZero() {
		from = until.Add(-2 * time.Minute)
	}
	var gapErr error
	if until.Sub(from) > maxLogLag {
		skipped := until.Add(-maxLogLag)
		gapErr = fmt.Errorf("provider logs %s..%s skipped: cursor lag exceeded %s",
			from.Format(time.RFC3339), skipped.Format(time.RFC3339), maxLogLag)
		from = skipped
	}
	to := until
	if to.Sub(from) > maxLogWindow {
		to = from.Add(maxLogWindow)
	}
	if !to.After(from) {
		return gapErr
	}
	entries, to, err := r.readLogWindow(ctx, from, to)
	if err != nil && !errors.Is(err, ErrLogWindowSaturated) {
		return errors.Join(gapErr, err)
	}
	if deliverErr := r.deliver(ctx, entries, to); deliverErr != nil {
		return errors.Join(gapErr, deliverErr)
	}
	r.through = to
	return errors.Join(gapErr, err)
}

// readLogWindow fetches [from, to), halving the window while the
// provider reports saturation. If even the minimum window is saturated
// it returns ErrLogWindowSaturated with the minimum window's end so the
// caller still advances (and reports the loss) instead of retrying the
// same saturated window forever.
func (r *Runtime) readLogWindow(ctx context.Context, from, to time.Time) (
	[]logwatch.LogEntry, time.Time, error,
) {
	for {
		entries, err := r.api.Logs(ctx, r.database, from, to)
		if !errors.Is(err, ErrLogWindowSaturated) {
			return entries, to, err
		}
		if to.Sub(from) <= minLogWindow {
			return nil, to, fmt.Errorf("provider logs %s..%s dropped: %w",
				from.Format(time.RFC3339), to.Format(time.RFC3339), err)
		}
		half := from.Add(to.Sub(from) / 2).Truncate(time.Minute)
		if half.Sub(from) < minLogWindow {
			half = from.Add(minLogWindow)
		}
		to = half
	}
}

// deliver hands unseen entries to the handler and remembers them for
// overlap deduplication. The handler error keeps the cursor in place so
// the window is retried (bounded by maxLogLag).
func (r *Runtime) deliver(ctx context.Context, entries []logwatch.LogEntry,
	windowEnd time.Time,
) error {
	fresh := make([]logwatch.LogEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := r.seen[logIdentity(entry)]; !ok {
			fresh = append(fresh, entry)
		}
	}
	if len(fresh) > 0 {
		if err := r.handler(ctx, fresh); err != nil {
			return err
		}
	}
	for _, entry := range fresh {
		r.seen[logIdentity(entry)] = entry.Timestamp
	}
	for key, timestamp := range r.seen {
		if timestamp.Before(windowEnd.Add(-5 * time.Minute)) {
			delete(r.seen, key)
		}
	}
	return nil
}

func logIdentity(entry logwatch.LogEntry) [32]byte {
	encoded, _ := json.Marshal(entry) // LogEntry contains only JSON-supported scalar values.
	return sha256.Sum256(encoded)
}
