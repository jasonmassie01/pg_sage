package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pg-sage/sidecar/internal/store"
)

// security-09b: a managed (meta-DB) database may take a write-only action
// identity, a second role on the same database that runs changes and
// backend signals.

// actionInput maps the request's action fields to the store input: nil
// when neither is sent (unchanged), an empty username removes it.
func (r *dbCreateRequest) actionInput() *store.ActionCredentialInput {
	if r.ActionUsername == nil && r.ActionPassword == "" {
		return nil
	}
	a := &store.ActionCredentialInput{Password: r.ActionPassword}
	if r.ActionUsername != nil {
		a.Username = *r.ActionUsername
	}
	return a
}

// actionIdentityView is what GET shows of the action identity: whether one
// is configured and its role, never the password.
func actionIdentityView(rec store.DatabaseRecord) map[string]any {
	return map[string]any{
		"configured": rec.ActionUsername != "",
		"username":   rec.ActionUsername,
	}
}

// writeDatabaseInputError answers a refused create or update: 400 with
// code invalid_action_identity for the action identity, plain 400 for
// other validation. It reports whether it wrote a response. Store
// validation messages never carry a password.
func writeDatabaseInputError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrActionIdentity):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "invalid_action_identity", "error": err.Error()})
		return true
	case errors.Is(err, store.ErrValidation):
		jsonError(w, err.Error(), http.StatusBadRequest)
		return true
	}
	return false
}

// actionTestConnString derives the action role's connection string from
// the observation one: same host, port, database and options, another
// role. Only URL connection strings are accepted.
func actionTestConnString(observation, user, password string) (string, error) {
	if user == "" {
		return "", errors.New("the action identity needs a role")
	}
	u, err := url.Parse(observation)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("the observation connection is not a postgres URL")
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

// testRequestedActionIdentity tests the action role a request names with
// both fields, on the observation connection's host; nil otherwise.
func testRequestedActionIdentity(
	ctx context.Context, req dbCreateRequest, observation string,
) *ConnectionTestResult {
	if req.ActionUsername == nil || strings.TrimSpace(*req.ActionUsername) == "" ||
		req.ActionPassword == "" {
		return nil
	}
	connStr, err := actionTestConnString(observation, *req.ActionUsername, req.ActionPassword)
	if err != nil {
		return &ConnectionTestResult{Status: "error", Error: err.Error()}
	}
	return testFromConnStringAtIP(ctx, connStr, req.resolvedIP)
}

// testManagedActionIdentity tests a managed database's action role: the
// one a preview body names, or else, without a body, the stored one.
func testManagedActionIdentity(
	ctx context.Context, deps *DatabaseDeps, id int, req dbCreateRequest,
	hasBody bool, observation string,
) *ConnectionTestResult {
	if hasBody {
		return testRequestedActionIdentity(ctx, req, observation)
	}
	connStr, err := deps.Store.GetActionConnectionString(ctx, id)
	if err != nil {
		return &ConnectionTestResult{Status: "error",
			Error: fmt.Sprintf("the stored action credential of database %d cannot be "+
				"read; re-enter it", id)}
	}
	if connStr == "" {
		return nil
	}
	return testFromConnStringAtIP(ctx, connStr, "")
}

// csvHeaderProblem explains a CSV header the import refuses; "" accepts it.
func csvHeaderProblem(header []string) string {
	if hasActionColumns(header) {
		return "action identity columns are not imported: set action_username " +
			"and action_password per database through the API"
	}
	if !validCSVHeader(header) {
		return "invalid CSV header: expected " +
			"name,host,port,database_name,username,password,sslmode"
	}
	return ""
}

// hasActionColumns reports whether a CSV header names an action column:
// the import never takes credentials for a second role.
func hasActionColumns(header []string) bool {
	for _, col := range header {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(col)), "action_") {
			return true
		}
	}
	return false
}
