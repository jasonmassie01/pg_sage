package principalfile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// bindingCreator records who bound an identity through a file.
const bindingCreator = "principals-file"

// CoreBackend is the Backend over core's principal store (agentguard), the
// E2 identity bindings and the file-ownership table
// sage.guard_principal_files. Sponsors are pg_sage users by email.
type CoreBackend struct {
	store *agentguard.Store
	pool  *pgxpool.Pool
}

// NewCoreBackend returns the backend on the control pool.
func NewCoreBackend(pool *pgxpool.Pool) *CoreBackend {
	return &CoreBackend{store: agentguard.NewStore(pool), pool: pool}
}

// List returns every principal (all statuses) with its sponsor's email,
// owning file and identities.
func (c *CoreBackend) List(ctx context.Context) ([]Principal, error) {
	var all []agentguard.Principal
	opts := agentguard.ListOptions{Limit: 200}
	for {
		page, err := c.store.List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list principals: %w", err)
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			break
		}
		opts.Cursor = page.NextCursor
	}
	extra, err := c.extras(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Principal, 0, len(all))
	for _, p := range all {
		out = append(out, extra.principal(p))
	}
	return out, nil
}

// extras are the per-principal facts outside core's Principal.
type extras struct {
	emails     map[int]string
	owners     map[string]string
	identities map[string][]Identity
}

func (e extras) principal(p agentguard.Principal) Principal {
	out := Principal{ID: p.ID, Name: p.Name, Profile: p.Profile,
		EnvCeiling: string(p.EnvCeiling), Tenant: p.Tenant, Status: string(p.Status),
		CreatedBy: p.CreatedBy, Identities: e.identities[p.ID]}
	if p.SponsorUserID != nil {
		out.Sponsor = e.emails[*p.SponsorUserID]
	}
	if file, ok := e.owners[p.ID]; ok {
		out.ManagedBy = "file:" + file
	}
	return out
}

const extrasSQL = `/* pg_sage principals_file v1 */
SELECT 'email', u.id::text, u.email, '' FROM sage.users u
 WHERE u.id IN (SELECT sponsor_user_id FROM sage.guard_principals)
UNION ALL
SELECT 'owner', f.principal_id, f.file_name, '' FROM sage.guard_principal_files f
UNION ALL
SELECT 'identity', b.principal_id, b.issuer, b.subject
  FROM sage.guard_identity_bindings b`

func (c *CoreBackend) extras(ctx context.Context) (extras, error) {
	e := extras{emails: map[int]string{}, owners: map[string]string{},
		identities: map[string][]Identity{}}
	rows, err := c.pool.Query(ctx, extrasSQL)
	if err != nil {
		return e, fmt.Errorf("read principal sponsors, owners and identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key, a, b string
		if err := rows.Scan(&kind, &key, &a, &b); err != nil {
			return e, fmt.Errorf("scan principal extras: %w", err)
		}
		switch kind {
		case "email":
			var id int
			_, _ = fmt.Sscan(key, &id)
			e.emails[id] = a
		case "owner":
			e.owners[key] = a
		case "identity":
			e.identities[key] = append(e.identities[key], Identity{Issuer: a, Subject: b})
		}
	}
	return e, rows.Err()
}

// Create creates the principal, freezes it when declared frozen, and
// records the file that owns it.
func (c *CoreBackend) Create(ctx context.Context, p Principal) (string, error) {
	sponsor, err := c.sponsorID(ctx, p.Sponsor)
	if err != nil {
		return "", err
	}
	created, err := c.store.Create(ctx, agentguard.CreateRequest{Name: p.Name,
		SponsorUserID: sponsor, Tenant: p.Tenant, Profile: p.Profile,
		EnvCeiling: agentguard.Env(p.EnvCeiling), CreatedBy: p.CreatedBy})
	if err != nil {
		return "", err
	}
	if p.Status == string(agentguard.StatusFrozen) {
		if _, err := c.store.SetStatus(ctx, created.ID, agentguard.StatusFrozen,
			"declared frozen in "+p.ManagedBy); err != nil {
			return created.ID, err
		}
	}
	return created.ID, c.own(ctx, created.ID, p.ManagedBy, p.CreatedBy)
}

// Update changes one field through core.
func (c *CoreBackend) Update(ctx context.Context, id string, ch Change) error {
	var patch agentguard.Patch
	switch ch.Field {
	case "profile":
		patch.Profile = &ch.To
	case "env_ceiling":
		env := agentguard.Env(ch.To)
		patch.EnvCeiling = &env
	case "sponsor":
		if ch.To == "" {
			return fmt.Errorf("a sponsor cannot be removed through the file; set another")
		}
		sponsor, err := c.sponsorID(ctx, ch.To)
		if err != nil {
			return err
		}
		patch.SponsorUserID = sponsor
	case "status":
		_, err := c.store.SetStatus(ctx, id, agentguard.Status(ch.To),
			"principals file: "+string(ch.Op))
		return err
	case "managed_by":
		return c.own(ctx, id, ch.To, bindingCreator)
	default:
		return fmt.Errorf("field %q cannot be changed through core", ch.Field)
	}
	_, err := c.store.Update(ctx, id, patch)
	return err
}

func (c *CoreBackend) sponsorID(ctx context.Context, email string) (*int, error) {
	if email == "" {
		return nil, nil
	}
	var id int
	err := c.pool.QueryRow(ctx, `/* pg_sage principals_file v1 */ SELECT id
		FROM sage.users WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("sponsor %q is not a pg_sage user", email)
	}
	if err != nil {
		return nil, fmt.Errorf("look up sponsor: %w", err)
	}
	return &id, nil
}

// own records that a file manages the principal.
func (c *CoreBackend) own(ctx context.Context, id, managedBy, actor string) error {
	file, ok := strings.CutPrefix(managedBy, "file:")
	if !ok || file == "" {
		return fmt.Errorf("owner %q is not a principals file", managedBy)
	}
	_, err := c.pool.Exec(ctx, `/* pg_sage principals_file v1 */
		INSERT INTO sage.guard_principal_files (principal_id, file_name, applied_by)
		VALUES ($1, $2, $3) ON CONFLICT (principal_id) DO UPDATE
		SET file_name = EXCLUDED.file_name, applied_by = EXCLUDED.applied_by,
		    applied_at = now()`, id, file, actor)
	if err != nil {
		return fmt.Errorf("record the owning file: %w", err)
	}
	return nil
}

// Bind binds an external identity (E2) to the principal.
func (c *CoreBackend) Bind(ctx context.Context, id string, ident Identity) error {
	_, err := c.pool.Exec(ctx, `/* pg_sage principals_file v1 */
		INSERT INTO sage.guard_identity_bindings (issuer, subject, principal_id,
		created_by) VALUES ($1, $2, $3, $4)`, ident.Issuer, ident.Subject, id,
		bindingCreator)
	if err != nil {
		return fmt.Errorf("bind %s|%s: %w", ident.Issuer, ident.Subject, err)
	}
	return nil
}

// Unbind removes a binding of the principal.
func (c *CoreBackend) Unbind(ctx context.Context, id string, ident Identity) error {
	_, err := c.pool.Exec(ctx, `/* pg_sage principals_file v1 */
		DELETE FROM sage.guard_identity_bindings
		WHERE issuer = $1 AND subject = $2 AND principal_id = $3`,
		ident.Issuer, ident.Subject, id)
	if err != nil {
		return fmt.Errorf("unbind %s|%s: %w", ident.Issuer, ident.Subject, err)
	}
	return nil
}

// CoreCatalog checks sponsors against sage.users and profiles against a
// profile set.
type CoreCatalog struct {
	pool     *pgxpool.Pool
	profiles map[string][]string
}

// NewCoreCatalog returns the catalog; profiles maps names to classes.
func NewCoreCatalog(pool *pgxpool.Pool, profiles map[string][]string) *CoreCatalog {
	return &CoreCatalog{pool: pool, profiles: profiles}
}

// ProfileClasses returns a profile's capability classes.
func (c *CoreCatalog) ProfileClasses(name string) ([]string, bool) {
	classes, ok := c.profiles[name]
	return classes, ok
}

// UserExists reports a pg_sage user with that email. A lookup failure
// counts as absent (the plan then refuses the sponsor).
func (c *CoreCatalog) UserExists(email string) bool {
	var one int
	err := c.pool.QueryRow(context.Background(), `/* pg_sage principals_file v1 */
		SELECT 1 FROM sage.users WHERE email = $1`, email).Scan(&one)
	return err == nil
}

// SpecProfiles are the default profiles of spec §9 (agents.profiles), used
// until profiles are configurable (G2).
func SpecProfiles() map[string][]string {
	return map[string][]string{
		"readonly-analyst": {"read"},
		"app-writer":       {"read", "write_insert", "write_update"},
		"coding-agent": {"read", "write_insert", "write_update", "write_delete",
			"ddl_additive", "ddl_locking", "sandbox"},
		"legacy": {"read"},
	}
}
