package api

import (
	"net/http"
	"strconv"
)

// authenticatedActor names the session user for audit fields; request
// bodies can never choose who approved or created something (SURF-17).
func authenticatedActor(r *http.Request) string {
	user := UserFromContext(r.Context())
	switch {
	case user == nil:
		return ""
	case user.Email != "":
		return user.Email
	case user.ID > 0:
		return "user:" + strconv.Itoa(user.ID)
	}
	return ""
}
