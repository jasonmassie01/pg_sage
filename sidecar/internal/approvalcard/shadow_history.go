package approvalcard

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/store"
)

// shadowHistory is the shadow record of the action's class (roadmap 1.4),
// nil when the class is not self-initiated or has no shadow decisions.
func (l Loader) shadowHistory(ctx context.Context, a store.QueuedAction) (*shadow.History,
	error) {
	family, class := shadow.ClassOf(actionTypeOf(a), a.ProposedSQL)
	if family == "" {
		return nil, nil
	}
	h, err := shadow.NewStore(l.Pool).History(ctx, class, int64(a.FindingID),
		shadow.Shape(a.ProposedSQL))
	if err != nil {
		return nil, fmt.Errorf("approvalcard: read shadow history: %w", err)
	}
	if h.Summary.Total == 0 {
		return nil, nil
	}
	return &h, nil
}
