package api

import (
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// syncIdPDatabaseGrants applies oauth.database_grant_mapping at an SSO
// login (apiauth-08d): the user's IdP grants become exactly what their
// groups give, in one audited transaction. Without a mapping it does
// nothing. A failure refuses the login (ErrIdPGrantSync, 503): signing in
// with a stale grant set could keep access the IdP has withdrawn.
func syncIdPDatabaseGrants(r *http.Request, pool *pgxpool.Pool, cfg *config.OAuthConfig,
	userID int, groups []string) error {
	if !auth.DatabaseGrantMappingConfigured(cfg) {
		return nil
	}
	_, err := auth.NewDatabaseGrantStore(pool).SyncIdPGrants(r.Context(), auth.IdPGrantSync{
		UserID: userID, Grants: auth.ResolveIdPGrants(cfg, groups),
		SourceIP: requestClientIP(r),
	})
	if err != nil {
		return fmt.Errorf("%w: user %d: %v", auth.ErrIdPGrantSync, userID, err)
	}
	return nil
}
