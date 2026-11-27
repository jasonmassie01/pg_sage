package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/auth"
)

// The transaction surface of the GitOps directory reconciler (cli-02a).
// Every call runs in the caller's transaction and reads or writes through
// the same checks as the API's. No call mints a token: tokens stay a
// person's action in the app.

// Principal owners: the API's rows and the reconciler's.
const (
	ManagedByAPI    = "api"
	ManagedByConfig = "config"
)

// ConfigChangeTTL is how long a widening a file asks for waits for a
// person; the reconciler asks again once it lapses.
const ConfigChangeTTL = 24 * time.Hour

// OwnedPrincipal is a principal with its owner.
type OwnedPrincipal struct {
	ID         string
	Name       string
	SponsorID  *int
	Tenant     string
	Profile    string
	EnvCeiling Env
	Status     Status
	ManagedBy  string
}

// OwnedPrincipalsTx lists every principal with its owner, by name, locked
// for the rest of tx.
func OwnedPrincipalsTx(ctx context.Context, tx pgx.Tx) ([]OwnedPrincipal, error) {
	rows, err := tx.Query(ctx, `/* pg_sage guard_principal_owned v1 */
		SELECT id, name, sponsor_user_id, tenant, profile, env_ceiling, status, managed_by
		FROM sage.guard_principals ORDER BY name FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("agentguard: listing principals for the config reconcile: %w",
			err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwnedPrincipal, error) {
		var p OwnedPrincipal
		var env, status string
		err := row.Scan(&p.ID, &p.Name, &p.SponsorID, &p.Tenant, &p.Profile, &env, &status,
			&p.ManagedBy)
		p.EnvCeiling, p.Status = Env(env), Status(status)
		return p, err
	})
}

// CreateOwnedTx creates a principal owned by managedBy and records
// agent_created; it returns the new id.
func CreateOwnedTx(ctx context.Context, tx pgx.Tx, req CreateRequest,
	managedBy string) (string, error) {
	req, err := req.normalize()
	if err != nil {
		return "", err
	}
	if managedBy != ManagedByAPI && managedBy != ManagedByConfig {
		return "", invalid("managed_by %q must be api or config", managedBy)
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	if err := createInTx(ctx, tx, id, req); err != nil {
		return "", err
	}
	return id, setOwner(ctx, tx, id, managedBy)
}

// AdoptTx moves a principal to the reconciler (a file's adopt: true).
func AdoptTx(ctx context.Context, tx pgx.Tx, id string, a Audit) error {
	cur, err := lockForChange(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := setOwner(ctx, tx, id, ManagedByConfig); err != nil {
		return err
	}
	return record(ctx, tx, a, auth.AuditAgentUpdated, cur.shape.SponsorUserID,
		map[string]any{"principal_id": id, "name": cur.name, "managed_by": ManagedByConfig,
			"reason": a.Reason})
}

func setOwner(ctx context.Context, tx pgx.Tx, id, managedBy string) error {
	if _, err := tx.Exec(ctx, `/* pg_sage guard_principal_owner v1 */
		UPDATE sage.guard_principals SET managed_by = $2 WHERE id = $1`,
		id, managedBy); err != nil {
		return fmt.Errorf("agentguard: marking principal %s %s-owned: %w", id, managedBy, err)
	}
	return nil
}

// UpdateNarrowingTx applies a patch that does not widen the principal as
// tx sees it locked; one that widens is ErrChangeWidens.
func UpdateNarrowingTx(ctx context.Context, tx pgx.Tx, id string, patch Patch,
	a Audit) error {
	if err := patch.validate(); err != nil {
		return err
	}
	if err := a.validate(auth.AuditAgentUpdated); err != nil {
		return err
	}
	if patch.empty() {
		return nil
	}
	return updateInTx(ctx, tx, id, patch, a, true)
}

// RetireTx retires a principal in tx. Its database roles are dropped by
// upkeep only under a person's recorded approval.
func RetireTx(ctx context.Context, tx pgx.Tx, id string, a Audit) error {
	if err := a.validate(auth.AuditAgentRetired); err != nil {
		return err
	}
	return setStatusInTx(ctx, tx, id, StatusRetired, a)
}

// RequestConfigChangeTx records a widening a config file asks for as the
// principal's pending change. No user requested it, so any admin who is
// not the agent's sponsor may confirm it; until one does nothing changes.
// actor must name the file ("config:<path>@<sha256>").
func RequestConfigChangeTx(ctx context.Context, tx pgx.Tx, id string, patch Patch,
	actor string, a Audit) (PendingChange, error) {
	if !strings.HasPrefix(actor, "config:") || a.ActorUserID != 0 {
		return PendingChange{}, invalid("a config change names its file and no user")
	}
	if err := patch.validate(); err != nil {
		return PendingChange{}, err
	}
	if patch.empty() {
		return PendingChange{}, invalid("a change must set sponsor_user_id, profile or " +
			"env_ceiling")
	}
	if err := checkText("actor", actor, 1, maxActorLen); err != nil {
		return PendingChange{}, err
	}
	if err := a.validate(auth.AuditAgentChangeRequested); err != nil {
		return PendingChange{}, err
	}
	return requestInTx(ctx, tx, ChangeRequest{PrincipalID: id, Patch: patch, Actor: actor,
		TTL: ConfigChangeTTL, Audit: a})
}

// PendingChangeTx is the principal's live pending change, or nil.
func PendingChangeTx(ctx context.Context, tx pgx.Tx, id string) (*Patch, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `/* pg_sage guard_principal_change_live v1 */
		SELECT patch FROM sage.guard_principal_changes
		WHERE principal_id = $1 AND status = 'pending' AND expires_at > now()
		ORDER BY id DESC LIMIT 1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading the pending change of %s: %w", id, err)
	}
	var doc PatchDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("agentguard: decoding the pending change of %s: %w", id, err)
	}
	patch := doc.patch()
	return &patch, nil
}

// SamePatch reports whether two patches change the same fields alike.
func SamePatch(a, b Patch) bool { return a.equal(b) }

// ValidateCreate checks a principal's fields the way Create would,
// without a database: name, profile, ceiling and tenant.
func ValidateCreate(name, tenant, profile string, ceiling Env) error {
	req := CreateRequest{Name: name, Tenant: tenant, Profile: profile, EnvCeiling: ceiling,
		CreatedBy: "config"}
	_, err := req.normalize()
	return err
}
