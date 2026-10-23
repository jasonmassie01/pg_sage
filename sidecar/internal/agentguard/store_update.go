package agentguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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

// Widens reports whether applying patch to p widens what p may do: a
// higher ceiling or another profile (§6.11). A sponsor change does not.
func (patch Patch) Widens(p Principal) bool {
	if patch.EnvCeiling != nil && patch.EnvCeiling.Rank() > p.EnvCeiling.Rank() {
		return true
	}
	return patch.Profile != nil && *patch.Profile != p.Profile
}

func (patch Patch) validate() error {
	switch {
	case patch.SponsorUserID != nil && *patch.SponsorUserID <= 0:
		return invalid("sponsor_user_id must be a positive user id")
	case patch.Profile != nil && !profilePattern.MatchString(*patch.Profile):
		return invalid("profile %q must be a profile name", *patch.Profile)
	case patch.EnvCeiling != nil && !patch.EnvCeiling.Valid():
		return invalid("env_ceiling %q must be branch, dev, stage or prod", *patch.EnvCeiling)
	}
	return nil
}

// Update applies patch to an active or frozen principal.
func (s *Store) Update(ctx context.Context, id string, patch Patch) (Principal, error) {
	if err := patch.validate(); err != nil {
		return Principal{}, err
	}
	var env *string
	if patch.EnvCeiling != nil {
		v := string(*patch.EnvCeiling)
		env = &v
	}
	return s.mutate(ctx, id, "updating", `/* pg_sage guard_principal_update v1 */
		UPDATE sage.guard_principals SET
			sponsor_user_id = COALESCE($2::int, sponsor_user_id),
			profile = COALESCE($3::text, profile),
			env_ceiling = COALESCE($4::text, env_ceiling), updated_at = now()
		WHERE id = $1 AND status <> 'retired' RETURNING id`,
		patch.SponsorUserID, patch.Profile, env)
}

// SetStatus moves a principal between active and frozen, or retires it.
// A retired principal never changes again. reason is kept for frozen.
func (s *Store) SetStatus(ctx context.Context, id string, to Status,
	reason string) (Principal, error) {
	if !to.Valid() {
		return Principal{}, invalid("status %q is not a principal status", to)
	}
	if err := checkText("reason", reason, 0, maxReasonLen); err != nil {
		return Principal{}, err
	}
	return s.mutate(ctx, id, "setting status of", `/* pg_sage guard_principal_status v1 */
		UPDATE sage.guard_principals SET status = $2,
			frozen_reason = CASE WHEN $2 = 'frozen' THEN $3 ELSE '' END, updated_at = now()
		WHERE id = $1 AND status <> 'retired' RETURNING id`, string(to), reason)
}

// mutate runs one UPDATE … RETURNING id and reloads the principal; no row
// means unknown (ErrNotFound) or retired (ErrRetired).
func (s *Store) mutate(ctx context.Context, id, what, sql string,
	args ...any) (Principal, error) {
	if !ValidID(id) {
		return Principal{}, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err := s.ready(); err != nil {
		return Principal{}, err
	}
	var got string
	err := s.pool.QueryRow(ctx, sql, append([]any{id}, args...)...).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return Principal{}, gerr
		}
		return Principal{}, fmt.Errorf("%w: principal %s", ErrRetired, id)
	}
	if err != nil {
		return Principal{}, mapWriteError(err, what+" principal "+id)
	}
	return s.Get(ctx, id)
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
