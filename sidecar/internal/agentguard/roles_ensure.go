package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Ensure runs guard_role_ensure: it creates the principal's two roles on
// the cluster, or re-asserts every attribute and setting of existing ones,
// through Executor.Apply under the operator's approval (L2). It needs
// PostgreSQL 16+, the encryption key, an active or frozen principal and a
// pg_sage role that passes the self-check. The roles are post-checked in
// the same transaction (attributes and settings equal the spec, no
// dangerous membership, owns nothing); a failed check rolls back.
func (m *RoleManager) Ensure(ctx context.Context, req RoleRequest) (RoleResult, error) {
	if err := m.prepare(ctx, req); err != nil {
		return RoleResult{}, err
	}
	stored, err := m.store.ClusterRolesOf(ctx, req.PrincipalID, req.Cluster.Key)
	if err != nil {
		return RoleResult{}, err
	}
	gateReq, err := policyRequest(executor.ActionTypeGuardRoleEnsure, req)
	if err != nil {
		return RoleResult{}, err
	}
	run := &ensureRun{m: m, req: req, haveCredential: stored != nil}
	actionID, err := req.Executor.Apply(ctx, executor.ActionIntent{Request: gateReq,
		Authorize: authorizer(req.Executor, gateReq), SlotHeld: true, Execute: run.execute})
	run.result.ActionID = actionID
	if err == nil {
		run.result.Ownership = ownershipAfter(ctx, req.Cluster)
	}
	return run.result, err
}

// prepare runs the checks every role contract shares.
func (m *RoleManager) prepare(ctx context.Context, req RoleRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	if m.keyring == nil {
		return ErrEncryptionKeyRequired
	}
	p, err := m.store.Get(ctx, req.PrincipalID)
	if err != nil {
		return err
	}
	if p.Retired() {
		return fmt.Errorf("%w: principal %s", ErrRetired, p.ID)
	}
	v, err := serverVersion(ctx, req.Cluster.Admin)
	if err != nil {
		return err
	}
	if v < MinRoleServerVersion {
		return fmt.Errorf("%w: server version %d", ErrRoleManagementUnsupported, v)
	}
	self, err := SelfCheck(ctx, req.Cluster.Admin)
	if err != nil {
		return err
	}
	return self.Err()
}

// ensureRun is one guard_role_ensure moving through Apply.
type ensureRun struct {
	m              *RoleManager
	req            RoleRequest
	haveCredential bool
	result         RoleResult
	statements     []string // as recorded: verifiers redacted
}

func (r *ensureRun) execute(ctx context.Context,
	decision executor.ActionPolicyDecision) (int64, error) {
	secret, err := r.applyRoles(ctx)
	if err != nil {
		return 0, err
	}
	login, broker := LoginRoleName(r.req.PrincipalID), BrokerRoleName(r.req.PrincipalID)
	if secret != "" {
		cr := ClusterRole{PrincipalID: r.req.PrincipalID, ClusterKey: r.req.Cluster.Key,
			LoginRole: login, BrokerRole: broker}
		if err := r.m.store.saveClusterRole(ctx, r.m.keyring, cr, secret); err != nil {
			return 0, fmt.Errorf("roles of %s were set but their credential was not "+
				"stored; the broker login fails until the next ensure: %w",
				r.req.PrincipalID, err)
		}
	}
	return recordRoleAction(ctx, r.req, decision, executor.ActionTypeGuardRoleEnsure,
		r.statements, map[string]any{"created": r.result.Created,
			"rotated": r.result.Rotated, "skipped_settings": r.result.SkippedSettings})
}

// applyRoles runs the role transaction and returns the new broker secret
// ("" when the stored one stays).
func (r *ensureRun) applyRoles(ctx context.Context) (string, error) {
	tx, err := r.req.Cluster.Admin.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("agentguard: beginning role transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockRoleTx(ctx, tx, r.m.cfg, r.req.PrincipalID); err != nil {
		return "", err
	}
	v, err := serverVersion(ctx, tx)
	if err != nil {
		return "", err
	}
	secret, err := r.upsertRoles(ctx, tx)
	if err != nil {
		return "", err
	}
	if err := r.applyDatabases(ctx, tx, v); err != nil {
		return "", err
	}
	if err := postCheckEnsure(ctx, tx, r.m.cfg, r.req, v, r.result.SkippedSettings); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("agentguard: committing role transaction: %w", err)
	}
	return secret, nil
}

// roleLockWait bounds the wait for another change to the same principal's
// roles (the contracts' "cluster role" lease) before the catalog statements
// run under the short role-change lock_timeout.
const roleLockWait = 30 * time.Second

// lockRoleTx sets the role transaction's guards: pg_sage gets SET (not
// INHERIT) on roles it creates, the per-principal advisory lock (waited for
// at most roleLockWait), then a short lock_timeout for the catalog.
func lockRoleTx(ctx context.Context, tx pgx.Tx, cfg RoleConfig, principalID string) error {
	steps := []struct {
		sql  string
		args []any
	}{
		{"SET LOCAL createrole_self_grant = 'set'", nil},
		{fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", roleLockWait.Milliseconds()), nil},
		{"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
			[]any{lockKey(principalID)}},
		{fmt.Sprintf("SET LOCAL lock_timeout = '%dms'",
			cfg.RoleChangeLockTimeout.Milliseconds()), nil},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, "/* pg_sage guard_role v1 */ "+st.sql, st.args...); err != nil {
			return fmt.Errorf("agentguard: locking roles of %s (%s): %w", principalID,
				st.sql, err)
		}
	}
	return nil
}

// upsertRoles creates or alters both roles.
func (r *ensureRun) upsertRoles(ctx context.Context, tx pgx.Tx) (string, error) {
	login, broker := LoginRoleName(r.req.PrincipalID), BrokerRoleName(r.req.PrincipalID)
	existing, err := existingRoles(ctx, tx, login, broker)
	if err != nil {
		return "", err
	}
	r.result.LoginRole, r.result.BrokerRole = login, broker
	r.result.Created = !existing[login] && !existing[broker]
	var secret, password string
	if !existing[broker] || !r.haveCredential || r.req.RotateCredential {
		if secret, err = newBrokerSecret(); err != nil {
			return "", err
		}
		verifier, err := newScramVerifier(secret)
		if err != nil {
			return "", err
		}
		password = " PASSWORD " + literal(verifier)
		r.result.Rotated = true
	}
	b := roleStatement(existing[broker], broker, true, r.m.cfg.BrokerConnectionLimit)
	if err := r.exec(ctx, tx, b, password); err != nil {
		return "", err
	}
	l := roleStatement(existing[login], login, false, r.m.cfg.ConnectionLimit)
	return secret, r.exec(ctx, tx, l, "")
}

func roleStatement(exists bool, role string, login bool, limit int) string {
	if exists {
		return "ALTER ROLE " + ident(role) + " WITH " + alterAttributes(login, limit)
	}
	return "CREATE ROLE " + ident(role) + " " + roleAttributes(login, limit)
}

// exec runs stmt+secretPart and records stmt (the secret part redacted).
func (r *ensureRun) exec(ctx context.Context, tx pgx.Tx, stmt, secretPart string) error {
	if _, err := tx.Exec(ctx, "/* pg_sage guard_role v1 */ "+stmt+secretPart); err != nil {
		return fmt.Errorf("agentguard: %s: %w", stmt, err)
	}
	if secretPart != "" {
		stmt += " PASSWORD '<scram verifier>'"
	}
	r.statements = append(r.statements, stmt)
	return nil
}

func existingRoles(ctx context.Context, tx pgx.Tx, names ...string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `/* pg_sage guard_role v1 */
		SELECT rolname::text FROM pg_catalog.pg_roles WHERE rolname = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading agent roles: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("agentguard: reading agent roles: %w", err)
		}
		out[n] = true
	}
	return out, rows.Err()
}

// applyDatabases sets each role's settings in every database of the
// cluster and grants the broker CONNECT. An optional setting the server
// refuses (42501) is skipped and recorded.
func (r *ensureRun) applyDatabases(ctx context.Context, tx pgx.Tx, version int) error {
	roles := []string{BrokerRoleName(r.req.PrincipalID), LoginRoleName(r.req.PrincipalID)}
	for _, d := range r.req.Cluster.Databases {
		for _, role := range roles {
			for _, s := range r.m.cfg.Settings(version) {
				if err := r.applySetting(ctx, tx, role, d.Name, s); err != nil {
					return err
				}
			}
		}
		if err := r.exec(ctx, tx, "ALTER ROLE "+ident(roles[1])+" IN DATABASE "+
			ident(d.Name)+" SET default_transaction_read_only = on", ""); err != nil {
			return err
		}
		if err := r.exec(ctx, tx, "GRANT CONNECT ON DATABASE "+ident(d.Name)+" TO "+
			ident(roles[0]), ""); err != nil {
			return err
		}
	}
	return nil
}

func (r *ensureRun) applySetting(ctx context.Context, tx pgx.Tx, role, db string,
	s Setting) error {
	stmt := "ALTER ROLE " + ident(role) + " IN DATABASE " + ident(db) + " SET " + s.Name +
		" = " + literal(s.Value)
	if !s.Optional {
		return r.exec(ctx, tx, stmt, "")
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("agentguard: savepoint for %s: %w", s.Name, err)
	}
	err = r.exec(ctx, sp, stmt, "")
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "42501" {
		_ = sp.Rollback(ctx)
		r.result.SkippedSettings = appendOnce(r.result.SkippedSettings, s.Name)
		return nil
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// recordRoleAction writes the action_log row of a role contract on the
// executor's database, with the principal's provenance.
func recordRoleAction(ctx context.Context, req RoleRequest,
	decision executor.ActionPolicyDecision, actionType string, statements []string,
	after map[string]any) (int64, error) {
	before, err := json.Marshal(map[string]any{"principal_id": req.PrincipalID,
		"cluster_key": req.Cluster.Key})
	if err != nil {
		return 0, fmt.Errorf("agentguard: encoding action state: %w", err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return 0, fmt.Errorf("agentguard: encoding action state: %w", err)
	}
	var id int64
	err = req.Cluster.Admin.QueryRow(ctx, `/* pg_sage guard_role_record v1 */
		INSERT INTO sage.action_log (action_type, sql_executed, before_state, after_state,
			outcome, approved_by, approved_at, decision_id, principal_id, approval_id,
			measured_at)
		VALUES ($1, $2, $3, $4, 'success', $5, now(), NULLIF($6::bigint, 0), $7,
			NULLIF($8::bigint, 0), now())
		RETURNING id`, actionType, strings.Join(statements, ";\n"), before, afterJSON,
		req.Approval.ApprovedBy, decision.DecisionID, req.PrincipalID,
		req.Approval.ApprovalID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agentguard: %s for %s ran but recording it failed: %w",
			actionType, req.PrincipalID, err)
	}
	return id, nil
}
