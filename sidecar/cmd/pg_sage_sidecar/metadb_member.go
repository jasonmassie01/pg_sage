package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// memberAction is a meta-DB member's action identity as stored
// (security-09b). The zero value is a single identity.
type memberAction struct {
	// dsn is the action role's connection string; for a credential no key
	// opens it is the role's address without a password.
	dsn string
	// err is set when the stored action password opens with no configured
	// key: the member observes and every change fails closed.
	err error
}

// readMemberAction reads a member's stored action identity. observation
// is the member's observation connection string.
func readMemberAction(
	ctx context.Context, st *store.DatabaseStore, rec store.DatabaseRecord, observation string,
) (memberAction, error) {
	dsn, err := st.GetActionConnectionString(ctx, rec.ID)
	return classifyMemberAction(dsn, err, rec.ActionUsername, observation)
}

// classifyMemberAction turns a stored action DSN read into a memberAction.
// Only ErrWrongEncryptionKey is kept as a fail-closed identity; any other
// read failure is returned.
func classifyMemberAction(
	dsn string, err error, actionUser, observation string,
) (memberAction, error) {
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		shut, shutErr := passwordlessRoleDSN(observation, actionUser)
		if shutErr != nil {
			return memberAction{}, errors.Join(err, shutErr)
		}
		return memberAction{dsn: shut, err: err}, nil
	}
	if err != nil {
		return memberAction{}, fmt.Errorf("get action connection string: %w", err)
	}
	return memberAction{dsn: dsn}, nil
}

// passwordlessRoleDSN is the observation address with another role and no
// password: the key of a fail-closed action pool, which never dials it.
func passwordlessRoleDSN(observation, user string) (string, error) {
	u, err := url.Parse(observation)
	if err != nil || u.Host == "" {
		return "", errors.New("the observation connection is not a URL")
	}
	u.User = url.User(user)
	return u.String(), nil
}

// open builds the member's action pool: nil for a single identity, the
// shared lazy pool (openActionPool) for a readable credential, and a pool
// that refuses every connection for one no key opens.
func (a memberAction) open(observation, name string) (*pgxpool.Pool, error) {
	if a.err == nil {
		return openActionPool(observation, config.DatabaseConfig{Name: name, ActionDSN: a.dsn})
	}
	pc, err := pgxpool.ParseConfig(a.dsn)
	if err != nil {
		return nil, fmt.Errorf("db %q: action identity address does not parse", name)
	}
	refusal := fmt.Errorf("%w: db %q: the stored action password does not open: %w",
		executor.ErrActionIdentityUnavailable, name, a.err)
	pc.MaxConns, pc.MinConns = 1, 0
	pc.BeforeConnect = func(context.Context, *pgx.ConnConfig) error { return refusal }
	p, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, fmt.Errorf("db %q: action identity pool: %w", name, err)
	}
	logWarn("meta-db", "db %q: %v; observation continues and every change fails until "+
		"the action password is re-entered", name, refusal)
	return p, nil
}

// runningActionDSN is the action connection string a running generation
// was built with; "" without an action identity.
func runningActionDSN(inst *fleet.DatabaseInstance) string {
	if inst == nil || inst.ActionPool == nil {
		return ""
	}
	return inst.ActionPool.Config().ConnString()
}

// metaActionChanged reports whether a connected generation runs another
// action identity than the record's. A failed placeholder has no pools:
// the reconnect loop reads the credential afresh.
func metaActionChanged(current *fleet.DatabaseInstance, action memberAction) bool {
	if current == nil || current.Pool == nil {
		return false
	}
	return runningActionDSN(current) != action.dsn
}

func prepareStoreDatabase(
	ctx context.Context, state *metaDBState, rec store.DatabaseRecord,
) (*fleet.DatabaseInstance, error) {
	connStr, err := state.Store.GetConnectionString(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("get connection string: %w", err)
	}
	action, err := readMemberAction(ctx, state.Store, rec, connStr)
	if err != nil {
		return nil, err
	}
	return prepareStoreDatabaseConnection(ctx, rec, connStr, action)
}

func prepareStoreDatabaseConnection(
	ctx context.Context, rec store.DatabaseRecord, connStr string, action memberAction,
) (*fleet.DatabaseInstance, error) {
	dbPool, err := connectMonitoredDBContext(ctx, connStr, rec.MaxConnections)
	if err != nil {
		return nil, err
	}
	actionPool, err := action.open(connStr, rec.Name)
	if err != nil {
		dbPool.Close()
		return nil, err
	}
	llmUsageIDs.setID(rec.Name, rec.ID) // fleet-10: usage rows keyed by id
	inst, err := buildStoreDatabaseRuntime(ctx, rec, dbPool, actionPool)
	if err != nil {
		dbPool.Close()
		closeActionPool(actionPool)
		return nil, err
	}
	return inst, nil
}

// buildStoreDatabaseRuntime builds a store-managed database's runtime. Its
// standing policy is scoped to the record and, with notifications, lives
// in the meta DB the API writes to (G5-B10, G5-B11). actionPool is the
// member's action identity, nil for one identity (security-09b).
func buildStoreDatabaseRuntime(
	ctx context.Context, rec store.DatabaseRecord, dbPool, actionPool *pgxpool.Pool,
) (*fleet.DatabaseInstance, error) {
	rt, err := buildDatabaseRuntime(ctx, databaseRuntimeSpec{
		Scope: "meta-db", Name: rec.Name, DatabaseID: rec.ID,
		Config: storeRecordToDBConfig(rec), Pool: dbPool, ActionPool: actionPool,
		ControlPool: notificationControlPool(globalMetaState, nil),
		ExecMode:    resolveExecMode(rec), Parent: shutdownCtx, RequireChecks: true,
	})
	if err != nil {
		return nil, err
	}
	return rt.inst, nil
}
