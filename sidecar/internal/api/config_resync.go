package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// GlobalDurableSource is the global revision a control database shares
// between replicas (config-05): the durable generation, and the current
// file base with every persisted global override replayed onto it, so
// overrides keep their precedence over the YAML.
type GlobalDurableSource struct {
	Store *store.ConfigStore
	// Base reloads the file config without overrides.
	Base func() (*config.Config, error)
}

// DurableGeneration is one bounded read of the global CAS generation.
func (s GlobalDurableSource) DurableGeneration(ctx context.Context) (uint64, error) {
	if s.Store == nil {
		return 0, fmt.Errorf("config store is unavailable")
	}
	return s.Store.GetGeneration(ctx, 0)
}

// DurableCandidate rebuilds the desired config the way startup does. A
// stored row the store would refuse today is skipped with a warning; the
// controller validates the whole.
func (s GlobalDurableSource) DurableCandidate(ctx context.Context) (*config.Config, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store is unavailable")
	}
	return globalCandidateWithoutOverride(
		ctx, s.Store, runtimeConfigBase(s.Base, nil, nil), "")
}

// writeControlledApplyError answers a refused global write. On a generation
// conflict this replica first resyncs with the durable revision, so the 409
// carries the fresh generation and the client's refetch-and-retry succeeds
// instead of failing forever (config-05). The CAS itself is unchanged: the
// write is still refused.
func writeControlledApplyError(
	w http.ResponseWriter, r *http.Request,
	controller *config.ConfigController, err error,
) {
	if !errors.Is(err, config.ErrGenerationConflict) &&
		!errors.Is(err, store.ErrConfigGenerationConflict) {
		writeConfigApplyError(w, r, err)
		return
	}
	resyncAfterConflict(r.Context(), controller)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":              err.Error(),
		"desired_generation": controller.Desired().Generation,
		"active_generation":  controller.Active().Generation,
	})
}

// resyncAfterConflict never changes the answer: a failed resync leaves this
// replica's generation in the 409 and the next poll tries again.
func resyncAfterConflict(ctx context.Context, controller *config.ConfigController) {
	result, adopted, err := controller.ResyncFromDurable(ctx)
	if err != nil {
		slog.Warn("config resync after a generation conflict failed; "+
			"answering with this replica's generation", "error", err)
		return
	}
	if adopted {
		slog.Info("config resynced after a generation conflict",
			"desired_generation", result.DesiredGeneration,
			"active_generation", result.ActiveGeneration)
	}
}

// lastResyncValue is when this replica last adopted another writer's
// generation (RFC 3339, UTC), or nil when it never has.
func lastResyncValue(controller *config.ConfigController) any {
	at := controller.LastResync()
	if at.IsZero() {
		return nil
	}
	return at.UTC().Format(time.RFC3339Nano)
}
