package agentguard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/auth"
)

// ListOptions page through principals by name (§8.1: limit ≤ 200,
// cursor). Cursor is the next_cursor of the previous page.
type ListOptions struct {
	Limit  int
	Cursor string
	Status Status // "" = every status
}

// Page is one page of principals; NextCursor is "" on the last page.
type Page struct {
	Items      []Principal `json:"items"`
	NextCursor string      `json:"next_cursor"`
}

// List returns principals ordered by name.
func (s *Store) List(ctx context.Context, opts ListOptions) (Page, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	switch {
	case limit < 0 || limit > maxLimit:
		return Page{}, invalid("limit must be 1 to %d", maxLimit)
	case opts.Status != "" && !opts.Status.Valid():
		return Page{}, invalid("status %q is not a principal status", opts.Status)
	case opts.Cursor != "" && !ValidName(opts.Cursor):
		return Page{}, invalid("cursor is not valid")
	}
	if err := s.ready(); err != nil {
		return Page{}, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage guard_principal_list v1 */
		SELECT `+principalColumns+principalFrom+`
		WHERE p.name > $1 AND ($2 = '' OR p.status = $2)
		ORDER BY p.name LIMIT $3`, opts.Cursor, string(opts.Status), limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("agentguard: listing principals: %w", err)
	}
	defer rows.Close()
	page := Page{Items: []Principal{}}
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return Page{}, fmt.Errorf("agentguard: reading principal row: %w", err)
		}
		page.Items = append(page.Items, p)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("agentguard: listing principals: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = page.Items[limit-1].Name
	}
	return page, nil
}

// Patch changes a principal's sponsor, profile or ceiling; nil fields stay.
// Whether a change widens (and so needs a second person) is the caller's
// decision (Widens).
type Patch struct {
	SponsorUserID *int
	Profile       *string
	EnvCeiling    *Env
}

// Widens reports whether applying patch to p widens what p may do
// (§6.11): a higher stored ceiling, or a profile that adds a class, a
// higher effective ceiling or the direct lane (agentgov-06). A sponsor
// change does not.
func (patch Patch) Widens(p Principal) bool {
	ceiling := p.EnvCeiling
	if patch.EnvCeiling != nil {
		if patch.EnvCeiling.Rank() > p.EnvCeiling.Rank() {
			return true
		}
		ceiling = *patch.EnvCeiling
	}
	if patch.Profile == nil || *patch.Profile == p.Profile {
		return false
	}
	return profileWidens(p.Profile, p.EnvCeiling, *patch.Profile, ceiling)
}

func (patch Patch) validate() error {
	switch {
	case patch.SponsorUserID != nil && *patch.SponsorUserID <= 0:
		return invalid("sponsor_user_id must be a positive user id")
	case patch.Profile != nil && !KnownProfile(*patch.Profile):
		return unknownProfile(*patch.Profile)
	case patch.EnvCeiling != nil && !patch.EnvCeiling.Valid():
		return invalid("env_ceiling %q must be branch, dev, stage or prod", *patch.EnvCeiling)
	}
	return nil
}

// empty reports whether the patch changes nothing.
func (patch Patch) empty() bool {
	return patch.SponsorUserID == nil && patch.Profile == nil && patch.EnvCeiling == nil
}

// apply is cur with the patch applied.
func (patch Patch) apply(cur shape) shape {
	if patch.SponsorUserID != nil {
		v := *patch.SponsorUserID
		cur.SponsorUserID = &v
	}
	if patch.Profile != nil {
		cur.Profile = *patch.Profile
	}
	if patch.EnvCeiling != nil {
		cur.EnvCeiling = string(*patch.EnvCeiling)
	}
	return cur
}

// movesSponsor reports whether the patch names another sponsor than cur's.
func (patch Patch) movesSponsor(cur shape) bool {
	return patch.SponsorUserID != nil &&
		(cur.SponsorUserID == nil || *cur.SponsorUserID != *patch.SponsorUserID)
}

// Update applies patch to an active or frozen principal and records
// agent_updated (or a.Event) in the same transaction. A new sponsor must
// be an enabled operator or admin, and the sponsor cannot change while the
// principal has an open freeze. An empty patch changes and records nothing.
func (s *Store) Update(ctx context.Context, id string, patch Patch,
	a Audit) (Principal, error) {
	return s.update(ctx, id, patch, a, false)
}

// UpdateNarrowing is Update for a patch the caller judged not to widen on
// an earlier read. Under the row lock it judges the patch again and
// refuses one that now widens with ErrChangeWidens (agentgov-06), so a
// change committed in between cannot turn it into an unconfirmed widening.
func (s *Store) UpdateNarrowing(ctx context.Context, id string, patch Patch,
	a Audit) (Principal, error) {
	return s.update(ctx, id, patch, a, true)
}

func (s *Store) update(ctx context.Context, id string, patch Patch, a Audit,
	narrowOnly bool) (Principal, error) {
	if err := patch.validate(); err != nil {
		return Principal{}, err
	}
	err := a.validate(auth.AuditAgentUpdated, auth.AuditAgentWidenedSingleOperator)
	if err != nil {
		return Principal{}, err
	}
	if !ValidID(id) {
		return Principal{}, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err := s.ready(); err != nil {
		return Principal{}, err
	}
	if patch.empty() {
		p, err := s.Get(ctx, id)
		if err == nil && p.Retired() {
			return Principal{}, fmt.Errorf("%w: principal %s", ErrRetired, id)
		}
		return p, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return updateInTx(ctx, tx, id, patch, a, narrowOnly)
	})
	if err != nil {
		return Principal{}, err
	}
	return s.Get(ctx, id)
}

func updateInTx(ctx context.Context, tx pgx.Tx, id string, patch Patch, a Audit,
	narrowOnly bool) error {
	cur, err := lockForChange(ctx, tx, id)
	if err != nil {
		return err
	}
	locked := Principal{ID: id, Profile: cur.shape.Profile,
		EnvCeiling: Env(cur.shape.EnvCeiling)}
	if narrowOnly && patch.Widens(locked) {
		return fmt.Errorf("%w: principal %s", ErrChangeWidens, id)
	}
	return applyLocked(ctx, tx, id, cur, patch, a, nil)
}

// checkSponsorMove refuses a new sponsor while the principal is frozen,
// or one who is not an enabled operator or admin.
func checkSponsorMove(ctx context.Context, tx pgx.Tx, id string, cur shape,
	patch Patch) error {
	if !patch.movesSponsor(cur) {
		return nil
	}
	if err := refuseWhileFrozen(ctx, tx, id); err != nil {
		return err
	}
	return checkSponsor(ctx, tx, *patch.SponsorUserID)
}

// applyLocked writes patch over the principal tx holds locked and records
// a.event(agent_updated) with extra merged into its detail.
func applyLocked(ctx context.Context, tx pgx.Tx, id string, cur lockedPrincipal,
	patch Patch, a Audit, extra map[string]any) error {
	if err := checkSponsorMove(ctx, tx, id, cur.shape, patch); err != nil {
		return err
	}
	next := patch.apply(cur.shape)
	_, err := tx.Exec(ctx, `/* pg_sage guard_principal_update v2 */
		UPDATE sage.guard_principals SET sponsor_user_id = $2, profile = $3,
			env_ceiling = $4, updated_at = now()
		WHERE id = $1`, id, next.SponsorUserID, next.Profile, next.EnvCeiling)
	if err != nil {
		return mapWriteError(err, "updating principal "+id)
	}
	detail := map[string]any{"principal_id": id, "name": cur.name, "before": cur.shape,
		"after": next, "reason": a.Reason}
	for k, v := range extra {
		detail[k] = v
	}
	return record(ctx, tx, a, a.event(auth.AuditAgentUpdated), next.SponsorUserID, detail)
}

// statusEvent is the event a move to status to records.
func statusEvent(to Status) string {
	if to == StatusRetired {
		return auth.AuditAgentRetired
	}
	return auth.AuditAgentStatusChanged
}

// SetStatus moves a principal between active and frozen, or retires it,
// and records the move in the same transaction. A retired principal never
// changes again. a.Reason is kept as the frozen reason.
func (s *Store) SetStatus(ctx context.Context, id string, to Status,
	a Audit) (Principal, error) {
	if !to.Valid() {
		return Principal{}, invalid("status %q is not a principal status", to)
	}
	if err := a.validate(statusEvent(to)); err != nil {
		return Principal{}, err
	}
	if !ValidID(id) {
		return Principal{}, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err := s.ready(); err != nil {
		return Principal{}, err
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return setStatusInTx(ctx, tx, id, to, a)
	})
	if err != nil {
		return Principal{}, err
	}
	return s.Get(ctx, id)
}

// setStatusInTx moves the principal tx locks to status to and records it.
func setStatusInTx(ctx context.Context, tx pgx.Tx, id string, to Status, a Audit) error {
	cur, err := lockForChange(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `/* pg_sage guard_principal_status v2 */
		UPDATE sage.guard_principals SET status = $2,
			frozen_reason = CASE WHEN $2 = 'frozen' THEN $3 ELSE '' END,
			updated_at = now()
		WHERE id = $1`, id, string(to), a.Reason); err != nil {
		return fmt.Errorf("agentguard: setting status of principal %s: %w", id, err)
	}
	return record(ctx, tx, a, statusEvent(to), cur.shape.SponsorUserID,
		map[string]any{"principal_id": id, "name": cur.name,
			"before_status": string(cur.status), "after_status": string(to),
			"reason": a.Reason})
}

// Taint marks a principal tainted by source (the read that caused it).
// taskID "" is the whole principal ("*"); only a trusted runtime task
// claim may name a task (§6.10). Re-tainting an uncleared row is a no-op;
// a cleared one is re-opened.
func (s *Store) Taint(ctx context.Context, id, taskID, source string) error {
	if taskID == "" {
		taskID = "*"
	}
	if err := checkText("source", source, 1, maxSourceLen); err != nil {
		return err
	}
	if err := checkText("task_id", taskID, 1, maxActorLen); err != nil {
		return err
	}
	if !ValidID(id) {
		return fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `/* pg_sage guard_taint v1 */
		INSERT INTO sage.guard_taint (principal_id, task_id, source) VALUES ($1, $2, $3)
		ON CONFLICT (principal_id, task_id, source) DO UPDATE
		SET tainted_at = now(), cleared_at = NULL, cleared_by = NULL
		WHERE sage.guard_taint.cleared_at IS NOT NULL`, id, taskID, source)
	if isFKViolation(err) {
		return fmt.Errorf("%w: principal %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("agentguard: tainting principal %s: %w", id, err)
	}
	return nil
}

// ClearTaint clears every open taint row of the principal and returns how
// many it cleared. by is "context_reset", "rotation" or a user's email.
func (s *Store) ClearTaint(ctx context.Context, id, by string) (int64, error) {
	if err := checkText("cleared_by", by, 1, maxActorLen); err != nil {
		return 0, err
	}
	if !ValidID(id) {
		return 0, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err := s.ready(); err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage guard_taint_clear v1 */
		UPDATE sage.guard_taint SET cleared_at = now(), cleared_by = $2
		WHERE principal_id = $1 AND cleared_at IS NULL`, id, by)
	if err != nil {
		return 0, fmt.Errorf("agentguard: clearing taint of %s: %w", id, err)
	}
	return tag.RowsAffected(), nil
}
