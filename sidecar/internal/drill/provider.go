package drill

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/clone"
)

// ProviderConfig says whose drill receipts a receipted provider writes.
type ProviderConfig struct {
	Adapter string
	Scope   string
	// Deployment reads the deployment id from the receipt database.
	Deployment func(context.Context) (string, error)
	Budget     Budget
	// AllowFullCopy is clone.allow_full_copy; the drill's own kinds list is
	// checked by the scheduler before a drill starts.
	AllowFullCopy     bool
	MaxCreatesPerHour int
	Rate              *clone.Rate
	// Lifetime ends the receipts' heartbeats; nil is context.Background().
	Lifetime context.Context
}

// NewReceiptedProvider wraps adapter so every drill clone is receipted
// (purpose drill) before the provider is asked for it, with a TTL of the
// drill budget plus an hour, this process as its holder and the shared
// clone.max_creates_per_hour and provider backoff.
func NewReceiptedProvider(adapter clone.Provider, receipts *clone.ReceiptStore,
	cfg ProviderConfig) (*clone.ReceiptingProvider, error) {
	if adapter == nil {
		return nil, errors.New("restore drill provider: clone adapter is required")
	}
	return clone.NewReceiptingProvider(adapter, receipts, clone.ReceiptingConfig{
		Adapter: cfg.Adapter, Scope: cfg.Scope, Deployment: cfg.Deployment,
		Purpose: clone.PurposeDrill, TTL: ReceiptTTL(cfg.Budget),
		AllowFullCopy: cfg.AllowFullCopy, MaxCreatesPerHour: cfg.MaxCreatesPerHour,
		Rate: cfg.Rate, Holder: clone.ProcessHolder(), Lifetime: cfg.Lifetime,
	})
}
