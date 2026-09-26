package agentdb

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgconn"
)

// LocalProvisioningEnabledFromEnv reports the explicit opt-in for
// local_postgres DDL. Without a dedicated meta-database the AgentDB store
// runs on a monitored database, so CREATE SCHEMA/DATABASE stays off unless
// PG_SAGE_AGENTDB_LOCAL_PROVISIONING=1 (G8-B17).
func LocalProvisioningEnabledFromEnv() bool {
	return os.Getenv("PG_SAGE_AGENTDB_LOCAL_PROVISIONING") == "1"
}

// EnableLocalProvisioning applies the explicit local provisioning opt-in.
func (s *Store) EnableLocalProvisioning(enabled bool) {
	if s != nil {
		s.opts.LocalProvisioning = enabled
	}
}

// SetRequireBackupBeforeDestroy applies agentdb.require_backup_before_destroy.
func (s *Store) SetRequireBackupBeforeDestroy(required bool) {
	if s != nil {
		s.opts.RequireBackupBeforeDestroy = required
	}
}

func (s *Store) ProvisionSchema(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	normalizeProviderFields(&req)
	if req.Provider != ProviderLocalPostgres || req.ProvisioningLevel != LevelSchema {
		return Deployment{}, ErrInvalid
	}
	req.SchemaName = sanitizeSchemaName(req.SchemaName)
	if req.SchemaName == "" {
		req.SchemaName = "agentdb_" + idFrom(req.TenantID, req.AgentID, req.DeploymentID)
	}
	if replay, done, err := s.prepareLocalDDL(ctx, &req); done || err != nil {
		return replay, err
	}
	if err := s.execLocalDDL(ctx, "CREATE SCHEMA "+quoteIdent(req.SchemaName)); err != nil {
		return Deployment{}, err
	}
	req.Metadata = cloneAnyMap(req.Metadata)
	req.Metadata["credential_scope"] = req.SchemaName
	req.ProvisioningStatus = "provisioned"
	req.ConnectionInfo = map[string]any{"provider": req.Provider, "schema_name": req.SchemaName}
	return s.registerLocal(ctx, req, "DROP SCHEMA "+quoteIdent(req.SchemaName))
}

func (s *Store) provisionLocalDatabase(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	req.DatabaseName = sanitizeDatabaseName(req.DatabaseName)
	if req.DatabaseName == "" {
		req.DatabaseName = "agentdb_" + idFrom(req.TenantID, req.AgentID, req.DeploymentID)
	}
	if replay, done, err := s.prepareLocalDDL(ctx, &req); done || err != nil {
		return replay, err
	}
	if err := s.execLocalDDL(ctx, "CREATE DATABASE "+quoteIdent(req.DatabaseName)); err != nil {
		return Deployment{}, err
	}
	req.Metadata = cloneAnyMap(req.Metadata)
	req.Metadata["credential_scope"] = req.DatabaseName
	req.ProvisioningStatus = "provisioned"
	req.ConnectionInfo = map[string]any{
		"provider": req.Provider, "database_name": req.DatabaseName,
	}
	return s.registerLocal(ctx, req, "DROP DATABASE "+quoteIdent(req.DatabaseName))
}

// prepareLocalDDL runs every check before any physical object is created:
// opt-in, emergency stop, registration validity, and idempotent replay of
// an existing deployment (which never re-runs DDL).
func (s *Store) prepareLocalDDL(
	ctx context.Context,
	req *RegisterRequest,
) (Deployment, bool, error) {
	if !s.opts.LocalProvisioning {
		return Deployment{}, true, fmt.Errorf("%w: local_postgres provisioning is disabled; "+
			"set PG_SAGE_AGENTDB_LOCAL_PROVISIONING=1 with a dedicated meta-database", ErrInvalid)
	}
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, true, err
	}
	if err := s.mutationAllowed(ctx); err != nil {
		return Deployment{}, true, err
	}
	probe := *req
	if err := normalizeRegister(&probe, s.opts.RequireBackupBeforeDestroy); err != nil {
		return Deployment{}, true, err
	}
	if _, err := s.Get(ctx, probe.DeploymentID); err == nil {
		dep, replayErr := s.existingRegistration(ctx, probe)
		return dep, true, replayErr
	} else if !errors.Is(err, ErrNotFound) {
		return Deployment{}, true, err
	}
	return Deployment{}, false, nil
}

// execLocalDDL creates a new object; an existing schema or database is
// never adopted (42P06 duplicate_schema, 42P04 duplicate_database).
func (s *Store) execLocalDDL(ctx context.Context, sql string) error {
	_, err := s.pool.Exec(ctx, sql)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42P06" || pgErr.Code == "42P04") {
		return fmt.Errorf("%w: %s already exists and is not owned by this deployment",
			ErrConflict, pgErr.Message)
	}
	return err
}

// registerLocal records ownership of a freshly created object and drops the
// object again if registration fails, so nothing is left unowned.
func (s *Store) registerLocal(
	ctx context.Context,
	req RegisterRequest,
	compensate string,
) (Deployment, error) {
	dep, err := s.Register(ctx, req)
	if err == nil {
		return dep, nil
	}
	if _, dropErr := s.pool.Exec(ctx, compensate); dropErr != nil {
		return Deployment{}, fmt.Errorf("%w (compensating drop failed: %v)", err, dropErr)
	}
	return Deployment{}, err
}
