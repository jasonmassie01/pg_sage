package envbind

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// DefaultPendingTTL is how long a widening label waits for its second admin.
const DefaultPendingTTL = 24 * time.Hour

// Database is one monitored database as the binder sees it. It carries no
// environment: the environment is derived, never claimed.
type Database struct {
	// ID is sage.sre_database_bindings.database_id.
	ID string
	// Name is the fleet name; a pg_sage-created sandbox is monitored under
	// its receipt name.
	Name string
	// Pool connects to the monitored database itself.
	Pool *pgxpool.Pool
	// ProviderRef is the provider resource id from cloud telemetry
	// ("<provider>:<resource>"), "" when none.
	ProviderRef string
}

// ReceiptSource finds a database's active clone receipt (clone.ReceiptStore).
type ReceiptSource interface {
	Active(ctx context.Context, name string) (clone.Receipt, bool, error)
}

// Binder evaluates and sets environment labels in the control database.
type Binder struct {
	control    *pgxpool.Pool
	receipts   ReceiptSource
	pendingTTL time.Duration
}

// Option configures a Binder.
type Option func(*Binder)

// WithPendingTTL sets how long a widening request waits for a second admin.
func WithPendingTTL(d time.Duration) Option {
	return func(b *Binder) {
		if d > 0 {
			b.pendingTTL = d
		}
	}
}

// NewBinder evaluates against control (nil: governance is posture-only and
// everything is prod); receipts may be nil (no branch can verify).
func NewBinder(control *pgxpool.Pool, receipts ReceiptSource, opts ...Option) *Binder {
	b := &Binder{control: control, receipts: receipts, pendingTTL: DefaultPendingTTL}
	for _, o := range opts {
		o(b)
	}
	return b
}

func prodBinding(db Database, reason string, live Identity) Binding {
	return Binding{Env: EnvProd, Evidence: Evidence{DatabaseID: db.ID, Database: db.Name,
		Label: EnvProd, Effective: EnvProd, Reasons: []string{reason}, Live: live,
		Strength: live.Strength()}}
}

// observe reads the live identity and the active receipt; a receipt
// supplies the provider resource id when telemetry has none.
func (b *Binder) observe(ctx context.Context, db Database) (Identity, *clone.Receipt,
	error) {
	live, err := ReadIdentity(ctx, db.Pool, db.ProviderRef)
	if err != nil {
		return live, nil, err
	}
	if b.receipts == nil {
		return live, nil, nil
	}
	r, ok, err := b.receipts.Active(ctx, db.Name)
	if err != nil {
		return live, nil, fmt.Errorf("envbind: read clone receipt of %s: %w", db.Name, err)
	}
	if !ok {
		return live, nil, nil
	}
	if live.ProviderRef == "" {
		live.ProviderRef, live.RefSource = r.ProviderRef(), RefReceipt
	}
	return live, &r, nil
}

// EnvironmentOf is the database's environment and its evidence. It re-reads
// the live identity on every call. Whenever anything cannot be read or
// verified the environment is prod; an error is returned only for a failed
// read, and the binding is prod then too.
func (b *Binder) EnvironmentOf(ctx context.Context, db Database) (Binding, error) {
	if b.control == nil {
		return prodBinding(db, ReasonNoControl, Identity{}), nil
	}
	if db.ID == "" {
		return prodBinding(db, ReasonUnbound, Identity{}), nil
	}
	live, receipt, err := b.observe(ctx, db)
	if err != nil {
		return prodBinding(db, ReasonIdentityUnreadable, live), err
	}
	return b.evaluateLive(ctx, b.control, db, live, receipt)
}

func (b *Binder) evaluateLive(ctx context.Context, q querier, db Database, live Identity,
	receipt *clone.Receipt) (Binding, error) {
	row, err := readRow(ctx, q, db.ID, false)
	if err != nil {
		return prodBinding(db, ReasonIdentityUnreadable, live), err
	}
	peers, err := readPeers(ctx, q, db.ID, live)
	if err != nil {
		return prodBinding(db, ReasonIdentityUnreadable, live), err
	}
	ev := evaluate(evalInput{Row: row, Live: live, Peers: peers, Receipt: receipt})
	ev.DatabaseID, ev.Database = db.ID, db.Name
	return Binding{Env: ev.Effective, Evidence: ev}, nil
}

// Record returns the stored label row of a database.
func (b *Binder) Record(ctx context.Context, databaseID string) (LabelRecord, bool, error) {
	if b.control == nil {
		return LabelRecord{}, false, ErrNoControl
	}
	r, err := readRow(ctx, b.control, databaseID, false)
	if err != nil || r == nil {
		return LabelRecord{}, false, err
	}
	return *r, true, nil
}

// SetResult is what a label change did: applied, or pending a second admin.
type SetResult struct {
	Applied bool        `json:"applied"`
	Pending bool        `json:"pending"`
	Record  LabelRecord `json:"record"`
}

func validActor(actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 200 || strings.IndexFunc(actor, unicode.IsControl) >= 0 {
		return "", ErrInvalidActor
	}
	return actor, nil
}

// SetLabel sets a database's label as actor (an admin). The live identity
// must verify it (else a *RefusedError). Narrowing applies at once;
// widening from the current effective environment needs a second admin,
// who repeats the same label within the pending TTL.
func (b *Binder) SetLabel(ctx context.Context, db Database, label Env, actor string) (
	SetResult, error) {
	if _, err := ParseEnv(string(label)); err != nil {
		return SetResult{}, err
	}
	actor, err := validActor(actor)
	if err != nil {
		return SetResult{}, err
	}
	if b.control == nil {
		return SetResult{}, ErrNoControl
	}
	if db.ID == "" {
		return SetResult{}, ErrUnbound
	}
	live, receipt, err := b.observe(ctx, db)
	if err != nil {
		return SetResult{}, err
	}
	var res SetResult
	err = pgx.BeginFunc(ctx, b.control, func(tx pgx.Tx) error {
		var txErr error
		res, txErr = b.setInTx(ctx, tx, db, label, actor, live, receipt)
		return txErr
	})
	return res, err
}

func (b *Binder) setInTx(ctx context.Context, tx pgx.Tx, db Database, label Env,
	actor string, live Identity, receipt *clone.Receipt) (SetResult, error) {
	if err := observeRow(ctx, tx, db.ID, live); err != nil {
		return SetResult{}, err
	}
	row, err := readRow(ctx, tx, db.ID, true)
	if err != nil {
		return SetResult{}, err
	}
	if row == nil {
		return SetResult{}, errors.New("envbind: label row vanished under its lock")
	}
	peers, err := readPeers(ctx, tx, db.ID, live)
	if err != nil {
		return SetResult{}, err
	}
	if err := checkLabel(label, live, peers, receipt); err != nil {
		return SetResult{}, err
	}
	current := evaluate(evalInput{Row: row, Live: live, Peers: peers, Receipt: receipt})
	if Widens(current.Effective, label) {
		fresh, err := pendingFresh(ctx, tx, db.ID, b.pendingTTL)
		if err != nil {
			return SetResult{}, err
		}
		if !fresh || row.PendingLabel != label || row.PendingBy == actor {
			rec, err := setPending(ctx, tx, db.ID, label, actor)
			return SetResult{Pending: true, Record: rec}, err
		}
	}
	rec, err := applyLabel(ctx, tx, db.ID, label, live, actor)
	return SetResult{Applied: true, Record: rec}, err
}
