package providerobs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

func (r *Runtime) pollLogs(ctx context.Context, now time.Time) error {
	if r.handler == nil {
		return nil
	}
	// Leave a minute for provider delivery, then overlap a minute to absorb late arrivals.
	until := now.UTC().Truncate(time.Minute).Add(-time.Minute)
	from := r.through.Add(-time.Minute)
	if r.through.IsZero() {
		from = until.Add(-2 * time.Minute)
	}
	entries, err := r.api.Logs(ctx, r.database, from, until)
	if err != nil {
		return err
	}
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
		if timestamp.Before(until.Add(-5 * time.Minute)) {
			delete(r.seen, key)
		}
	}
	r.through = until
	return nil
}

func logIdentity(entry logwatch.LogEntry) [32]byte {
	encoded, _ := json.Marshal(entry) // LogEntry contains only JSON-supported scalar values.
	return sha256.Sum256(encoded)
}
