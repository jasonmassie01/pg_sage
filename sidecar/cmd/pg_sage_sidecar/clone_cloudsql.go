package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// cloudSQLCloneProvider is the cloudsql_clone adapter (lab-05e): one Cloud
// SQL clone of the monitored instance per rehearsal, a full copy, through
// the Google Application Default Credentials chain (the token source
// cloud telemetry uses). monitored is the production database; without it
// the adapter can destroy but never creates. A clone lives at most the
// rehearsal receipt's TTL (restore deadline, rehearsal bound and margin),
// which is labelled on it.
func cloudSQLCloneProvider(cloneCfg config.CloneProviderConfig,
	monitored *pgxpool.Pool) (clonepkg.Provider, error) {
	s := cloneCfg.CloudSQL
	p, err := clonepkg.NewCloudSQLCloneProvider(clonepkg.CloudSQLCloneConfig{
		Project: s.Project, Instance: s.Instance,
		Deadline: restoreDeadline(cloneCfg), TTL: rehearsalReceiptTTL(cloneCfg),
		Token: cloudSQLToken, Production: cloudSQLProduction(monitored),
	})
	if err != nil {
		// A nil interface, not a typed nil: callers test the adapter for nil.
		return nil, err
	}
	return p, nil
}

// cloudSQLToken is the Google credential chain: an explicit
// GOOGLE_APPLICATION_CREDENTIALS key, the gcloud ADC file, then the
// metadata server.
func cloudSQLToken(ctx context.Context) (clonepkg.CloudSQLTokenSource, error) {
	tok, err := cloudtel.NewGCPTokenSource(ctx, cloudtel.GCPAuthOptions{
		CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")})
	if err != nil {
		// A nil interface, not a typed nil.
		return nil, err
	}
	return tok, nil
}

// cloudSQLProduction is the DSN pool was opened with: the monitored
// instance's IP and the users a clone reuses. nil for a nil pool.
func cloudSQLProduction(
	pool *pgxpool.Pool,
) func(context.Context) (clonepkg.CloudSQLProduction, error) {
	if pool == nil {
		return nil
	}
	return func(context.Context) (clonepkg.CloudSQLProduction, error) {
		return clonepkg.CloudSQLProduction{DSN: pool.Config().ConnString()}, nil
	}
}
