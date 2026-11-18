package store

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// SeedClassRules gives a fresh install's first alert channel the Page, Ask
// and Digest class rules (ux-09a), so it hears what matters before anyone
// builds a rule. An install that already has rules or other channels keeps
// them; it reports how many rules it wrote (3, 2 for a pager, or 0).
func (s *NotificationStore) SeedClassRules(ctx context.Context, channelID int) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return notify.SeedClassRules(qctx, s.pool, channelID)
}
