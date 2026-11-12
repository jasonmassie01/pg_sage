package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

// allowChatDatabase applies database grants (apiauth-08) to a chat
// decision: the mapped user must be an operator on the card's database.
// A refusal is audited, answered in the chat and leaves the card unused,
// so a user who may decide it still can.
func (d *chatopsDeps) allowChatDatabase(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, a chatops.Action, user auth.User, database string) bool {
	if d.authz == nil {
		return true
	}
	role, err := d.authz.roleOn(r.Context(), &user, database)
	if err != nil {
		slog.Error("database grant check failed; refusing the chat decision",
			"user_id", user.ID, "database", database, "error", err)
		reply(ch, a, "Not decided: pg_sage cannot check database grants right now. "+
			"Try again.")
		writeGrantStoreUnavailable(w)
		return false
	}
	if auth.RoleAtLeast(role, auth.RoleOperator) {
		return true
	}
	recordAuthEvent(r, d.pool, auth.AuthAuditEvent{Event: auth.AuditDatabaseForbidden,
		ActorUserID: user.ID, TargetUserID: user.ID, Detail: map[string]any{
			"database": database, "surface": "chatops", "provider": a.Provider}})
	reply(ch, a, fmt.Sprintf("Refused: you are not an operator on %s in pg_sage. "+
		"Ask an admin for a grant.", database))
	writeDatabaseForbidden(w, database, auth.RoleOperator)
	return false
}
