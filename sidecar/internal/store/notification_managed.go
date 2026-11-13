package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/notify"
)

// ErrManagedByConfig marks a write to a channel or rule the alerting
// reconciler owns (alerting-06b): it comes from the config file's
// alerting: block, so it is changed there, not through the API. Handlers
// answer 409 {"error":"managed_by_config"}.
var ErrManagedByConfig = errors.New("managed_by_config")

// The managed_by of one row; a constant per table, so no SQL is built.
const (
	channelManagedBySQL = `/* pg_sage */ SELECT managed_by
		FROM sage.notification_channels WHERE id = $1`
	ruleManagedBySQL = `/* pg_sage */ SELECT managed_by
		FROM sage.notification_rules WHERE id = $1`
)

// validateAPIChannelName refuses the config: prefix, which names the
// reconciler's channels; a clash would make its next reconcile fail.
func validateAPIChannelName(name string) error {
	if notify.IsConfigChannelName(name) {
		return fmt.Errorf("%w: channel names starting with \"config:\" are reserved for "+
			"the alerting: block of the config file", ErrValidation)
	}
	return nil
}

// whyNotWritten explains a guarded write that matched no API row: the
// row is config-managed, or it does not exist.
func (s *NotificationStore) whyNotWritten(
	ctx context.Context, query, what string, id int,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var managedBy string
	err := s.pool.QueryRow(qctx, query, id).Scan(&managedBy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s %d", ErrNotFound, what, id)
	case err != nil:
		return fmt.Errorf("reading %s %d: %w", what, id, err)
	case managedBy == notify.ManagedByConfig:
		return fmt.Errorf("%s %d: %w", what, id, ErrManagedByConfig)
	}
	return fmt.Errorf("%w: %s %d", ErrNotFound, what, id)
}

func (s *NotificationStore) channelNotWritten(ctx context.Context, id int) error {
	return s.whyNotWritten(ctx, channelManagedBySQL, "channel", id)
}

func (s *NotificationStore) ruleNotWritten(ctx context.Context, id int) error {
	return s.whyNotWritten(ctx, ruleManagedBySQL, "rule", id)
}
