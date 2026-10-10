package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// Cluster role statuses (sage.guard_cluster_roles.status).
const (
	RoleStatusActive  = "active"
	RoleStatusKilled  = "killed"
	RoleStatusRetired = "retired"
)

// ClusterRole is a principal's pair of roles on one cluster. The sealed
// broker credential is never part of it; BrokerCredential opens it.
type ClusterRole struct {
	PrincipalID string          `json:"principal_id"`
	ClusterKey  string          `json:"cluster_key"`
	LoginRole   string          `json:"login_role"`
	BrokerRole  string          `json:"broker_role"`
	KeyID       string          `json:"key_id"`
	ValidUntil  *time.Time      `json:"valid_until,omitempty"`
	PriorAttrs  json.RawMessage `json:"prior_attrs,omitempty"`
	Status      string          `json:"status"`
	RotatedAt   time.Time       `json:"rotated_at"`
}

const clusterRoleColumns = `principal_id, cluster_key, login_role, broker_role, key_id,
	valid_until, prior_attrs, status, rotated_at`

func scanClusterRole(row pgx.Row) (ClusterRole, error) {
	var r ClusterRole
	var prior []byte
	err := row.Scan(&r.PrincipalID, &r.ClusterKey, &r.LoginRole, &r.BrokerRole, &r.KeyID,
		&r.ValidUntil, &prior, &r.Status, &r.RotatedAt)
	if len(prior) > 0 {
		r.PriorAttrs = prior
	}
	return r, err
}

func (s *Store) queryClusterRoles(ctx context.Context, where string,
	args ...any) ([]ClusterRole, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage guard_cluster_roles v1 */
		SELECT `+clusterRoleColumns+` FROM sage.guard_cluster_roles WHERE `+where+`
		ORDER BY principal_id, cluster_key`, args...)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading cluster roles: %w", err)
	}
	defer rows.Close()
	out := []ClusterRole{}
	for rows.Next() {
		r, err := scanClusterRole(rows)
		if err != nil {
			return nil, fmt.Errorf("agentguard: reading cluster role row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentguard: reading cluster roles: %w", err)
	}
	return out, nil
}

// ClusterRoles lists a principal's roles on every cluster.
func (s *Store) ClusterRoles(ctx context.Context, principalID string) ([]ClusterRole, error) {
	if !ValidID(principalID) {
		return nil, fmt.Errorf("%w: principal %q", ErrNotFound, principalID)
	}
	return s.queryClusterRoles(ctx, "principal_id = $1", principalID)
}

// ClusterRolesOn lists every principal's roles on one cluster (the kill
// switch's per-cluster step, the backend check).
func (s *Store) ClusterRolesOn(ctx context.Context, clusterKey string) ([]ClusterRole, error) {
	return s.queryClusterRoles(ctx, "cluster_key = $1", clusterKey)
}

// RoleByName finds the registration of an agent role name (either lane)
// on a cluster. An unregistered role is ErrNotFound.
func (s *Store) RoleByName(ctx context.Context, clusterKey, rolname string) (ClusterRole,
	error) {
	if !RolePattern.MatchString(rolname) {
		return ClusterRole{}, fmt.Errorf("%w: %q is not an agent role", ErrNotFound, rolname)
	}
	rs, err := s.queryClusterRoles(ctx,
		"cluster_key = $1 AND (login_role = $2 OR broker_role = $2)", clusterKey, rolname)
	if err != nil {
		return ClusterRole{}, err
	}
	if len(rs) == 0 {
		return ClusterRole{}, fmt.Errorf("%w: role %s on cluster %s", ErrNotFound, rolname,
			clusterKey)
	}
	return rs[0], nil
}

// SetClusterRoleStatus records a kill, a restore or a retirement of one
// principal's roles on a cluster, with the attributes they had before
// (prior_attrs, restored by guard_unfreeze). A nil prior keeps the stored one.
func (s *Store) SetClusterRoleStatus(ctx context.Context, principalID, clusterKey,
	status string, prior json.RawMessage) error {
	switch status {
	case RoleStatusActive, RoleStatusKilled, RoleStatusRetired:
	default:
		return invalid("cluster role status %q", status)
	}
	if err := s.ready(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage guard_cluster_role_status v1 */
		UPDATE sage.guard_cluster_roles SET status = $3,
			prior_attrs = COALESCE($4::jsonb, prior_attrs)
		WHERE principal_id = $1 AND cluster_key = $2`, principalID, clusterKey, status,
		nullJSON(prior))
	if err != nil {
		return fmt.Errorf("agentguard: setting cluster role status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: roles of %s on %s", ErrNotFound, principalID, clusterKey)
	}
	return nil
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// credentialAAD binds a sealed broker credential to its row (§6.4).
func credentialAAD(principalID, clusterKey, role string) string {
	return principalID + clusterKey + role
}

// saveClusterRole upserts the roles of a principal on a cluster with a
// freshly sealed broker credential.
func (s *Store) saveClusterRole(ctx context.Context, kr *crypto.Keyring, r ClusterRole,
	secret string) error {
	if kr == nil {
		return ErrEncryptionKeyRequired
	}
	sealed, err := kr.Seal(secret, credentialAAD(r.PrincipalID, r.ClusterKey, r.BrokerRole))
	if err != nil {
		return fmt.Errorf("agentguard: sealing broker credential: %w", err)
	}
	if err := s.ready(); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `/* pg_sage guard_cluster_role_save v1 */
		INSERT INTO sage.guard_cluster_roles (principal_id, cluster_key, login_role,
			broker_role, broker_secret_ct, key_id, valid_until, status, rotated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', now())
		ON CONFLICT (principal_id, cluster_key) DO UPDATE SET
			broker_secret_ct = EXCLUDED.broker_secret_ct, key_id = EXCLUDED.key_id,
			valid_until = EXCLUDED.valid_until, status = 'active', rotated_at = now()`,
		r.PrincipalID, r.ClusterKey, r.LoginRole, r.BrokerRole, []byte(sealed),
		kr.ActiveKeyID(), r.ValidUntil)
	if err != nil {
		return fmt.Errorf("agentguard: saving cluster roles of %s: %w", r.PrincipalID, err)
	}
	return nil
}

// BrokerCredential opens the broker login of a principal on a cluster:
// the role name and its password, for the brokered pool only. The
// password is never logged or returned through the API or MCP.
func (s *Store) BrokerCredential(ctx context.Context, kr *crypto.Keyring, principalID,
	clusterKey string) (role, password string, err error) {
	if kr == nil {
		return "", "", ErrEncryptionKeyRequired
	}
	if err := s.ready(); err != nil {
		return "", "", err
	}
	var sealed []byte
	var status string
	err = s.pool.QueryRow(ctx, `/* pg_sage guard_broker_credential v1 */
		SELECT broker_role, broker_secret_ct, status FROM sage.guard_cluster_roles
		WHERE principal_id = $1 AND cluster_key = $2`, principalID, clusterKey).
		Scan(&role, &sealed, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: roles of %s on %s", ErrNotFound, principalID,
			clusterKey)
	}
	if err != nil {
		return "", "", fmt.Errorf("agentguard: reading broker credential: %w", err)
	}
	if status != RoleStatusActive {
		return "", "", &DeniedError{Reason: ReasonFrozen,
			Detail: "the broker login is " + status}
	}
	password, _, err = kr.Open(string(sealed), credentialAAD(principalID, clusterKey, role))
	if err != nil {
		return "", "", fmt.Errorf("agentguard: opening broker credential: %w", err)
	}
	return role, password, nil
}

// ClusterRolesOf returns a principal's roles on one cluster, nil when none
// are registered.
func (s *Store) ClusterRolesOf(ctx context.Context, principalID,
	clusterKey string) (*ClusterRole, error) {
	rs, err := s.queryClusterRoles(ctx, "principal_id = $1 AND cluster_key = $2",
		principalID, clusterKey)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}
