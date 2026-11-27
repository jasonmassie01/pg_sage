package approvalcard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// Backend-signal cards (policy-08). A cancel or terminate acts on one
// backend incarnation whose evidence is minutes old: its card lives at
// most BackendSignalCardTTL, offers no Snooze, and its hash covers the
// target identity its finding records (pid plus backend_start), so a
// reused pid is another card.

// BackendSignalCardTTL bounds a backend-signal card; the queue item's own
// expiry bounds it further.
const BackendSignalCardTTL = 15 * time.Minute

// sreFindingSource marks the finding of a Sage SRE proposal.
const sreFindingSource = "sage_sre"

// BackendSignal reports whether an action type signals a backend.
func BackendSignal(actionType string) bool {
	return actionType == "cancel_backend" || actionType == "terminate_backend"
}

// SignalsBackend reports whether a queue row signals a backend: by its
// action_type or, for an untyped row, by its SQL.
func SignalsBackend(a store.QueuedAction) bool {
	return BackendSignal(actionTypeOf(a))
}

// CardRefOf is the reference a card's chat delivery carries.
func CardRefOf(c Card) notify.CardRef {
	ref := notify.CardRef{Database: c.Database, QueueID: c.QueueID, CardHash: c.CardHash,
		Title: c.Title, Summary: Summary(c), ExpiresAt: c.ExpiresAt}
	if BackendSignal(c.ActionType) {
		ref.MaxTTL, ref.NoSnooze = BackendSignalCardTTL, true
	}
	return ref
}

// signalTarget is the backend incarnation a finding names; empty fields
// when it names none (the hash still covers that absence).
type signalTarget struct {
	pid   string
	start string
}

func signalTargetOf(f *FindingRow) (signalTarget, bool) {
	if f == nil {
		return signalTarget{}, false
	}
	raw, ok := f.Detail["signal_target"].(map[string]any)
	if !ok {
		return signalTarget{}, false
	}
	var t signalTarget
	if pid, ok := number(raw["pid"]); ok && pid > 0 && pid == float64(int64(pid)) {
		t.pid = strconv.FormatInt(int64(pid), 10)
	}
	t.start, _ = raw["backend_start"].(string)
	return t, t.pid != "" && t.start != ""
}

func (t signalTarget) String() string {
	return fmt.Sprintf("backend pid %s started %s", t.pid, t.start)
}

// cardHash binds a decision to the card's content: the queue row's
// content hash, plus the signal target for a backend signal.
func cardHash(a store.QueuedAction, f *FindingRow) string {
	base := ContentHash(a)
	if !SignalsBackend(a) {
		return base
	}
	t, _ := signalTargetOf(f)
	sum := sha256.New()
	for _, part := range []string{"approval-card-signal-v1", base, t.pid, t.start} {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// CurrentHash is a queue row's card hash as of now. A backend signal reads
// its finding's signal target from pool; any other action needs no read.
func CurrentHash(ctx context.Context, pool *pgxpool.Pool,
	a store.QueuedAction) (string, error) {
	if !SignalsBackend(a) {
		return ContentHash(a), nil
	}
	if pool == nil {
		return "", errors.New("approvalcard: no database pool to read the signal target")
	}
	f, err := Loader{Pool: pool}.finding(ctx, a.FindingID)
	if err != nil {
		return "", fmt.Errorf("approvalcard: queue item %d: %w", a.ID, err)
	}
	return cardHash(a, f), nil
}

// sreRationale is a Sage SRE proposal's summary as the card's rationale.
func sreRationale(detail map[string]any) *Rationale {
	if source, _ := detail["source"].(string); source != sreFindingSource {
		return nil
	}
	text, _ := detail["summary"].(string)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &Rationale{Source: "rule", Text: truncate(text, 1200)}
}
