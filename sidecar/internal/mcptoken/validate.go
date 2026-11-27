package mcptoken

import (
	"fmt"
	"regexp"
	"sort"
	"unicode"
	"unicode/utf8"
)

const (
	maxNameLen     = 100
	maxDatabaseLen = 63 // PostgreSQL identifier limit (bytes)
	maxActorLen    = 200
	allDatabases   = "*"
)

// principalIDPattern is an agent principal id (sage.guard_principals.id).
var principalIDPattern = regexp.MustCompile(`^agp_[a-z2-7]{20}$`)

// scopeOrder is the canonical scope order: read, propose, approve.
var scopeOrder = map[string]int{ScopeRead: 0, ScopePropose: 1, ScopeApprove: 2}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// normalize validates req and returns it with scopes and databases
// deduplicated and in canonical order. It never touches the database.
func normalize(req CreateRequest) (CreateRequest, error) {
	if err := checkText("name", req.Name, maxNameLen); err != nil {
		return CreateRequest{}, err
	}
	if req.Kind != KindAgent && req.Kind != KindOperator {
		return CreateRequest{}, invalid("kind %q must be agent or operator", req.Kind)
	}
	scopes, err := normalizeScopes(req.Scopes)
	if err != nil {
		return CreateRequest{}, err
	}
	if err := checkOwner(req, scopes); err != nil {
		return CreateRequest{}, err
	}
	databases, err := normalizeDatabases(req.Databases)
	if err != nil {
		return CreateRequest{}, err
	}
	if scopes[0] == ScopeMetrics && databases[0] != allDatabases {
		// /metrics renders every database's series: it cannot be narrowed.
		return CreateRequest{}, invalid("databases must be [\"*\"] for a metrics token: " +
			"/metrics covers every database")
	}
	if req.ExpiresIn < MinLifetime || req.ExpiresIn > MaxLifetime {
		return CreateRequest{}, invalid("expires_in %s must be between %s and %s",
			req.ExpiresIn, MinLifetime, MaxLifetime)
	}
	if err := checkText("created_by", req.CreatedBy, maxActorLen); err != nil {
		return CreateRequest{}, err
	}
	if err := req.Audit.validate(); err != nil {
		return CreateRequest{}, err
	}
	req.Scopes, req.Databases = scopes, databases
	return req, nil
}

const maxSourceIPLen = 200

// validate checks the audit's shape.
func (a Audit) validate() error {
	if a.ActorUserID < 0 {
		return invalid("audit actor_user_id must not be negative")
	}
	if len(a.SourceIP) > maxSourceIPLen || hasControl(a.SourceIP) {
		return invalid("audit source_ip must be at most %d characters without control "+
			"characters", maxSourceIPLen)
	}
	return nil
}

// checkText requires 1..max characters and no control characters.
func checkText(field, value string, max int) error {
	if !utf8.ValidString(value) {
		return invalid("%s must be valid UTF-8", field)
	}
	if n := utf8.RuneCountInString(value); n < 1 || n > max {
		return invalid("%s must be 1 to %d characters", field, max)
	}
	if hasControl(value) {
		return invalid("%s must not contain control characters", field)
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func normalizeScopes(scopes []string) ([]string, error) {
	seen := map[string]bool{}
	for _, s := range scopes {
		if _, ok := scopeOrder[s]; !ok && s != ScopeMetrics {
			return nil, invalid("unknown scope %q (want read, propose, approve, or metrics "+
				"alone)", s)
		}
		seen[s] = true
	}
	if seen[ScopeMetrics] {
		if len(seen) != 1 {
			return nil, invalid("the metrics scope must be the token's only scope")
		}
		return []string{ScopeMetrics}, nil
	}
	if !seen[ScopeRead] {
		return nil, invalid("scopes must include read")
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return scopeOrder[out[i]] < scopeOrder[out[j]] })
	return out, nil
}

func checkOwner(req CreateRequest, scopes []string) error {
	approve := scopes[len(scopes)-1] == ScopeApprove
	switch {
	case req.Kind == KindAgent && scopes[0] == ScopeMetrics:
		return invalid("the metrics scope is only for operator tokens")
	case req.Kind == KindAgent && approve:
		return ErrApproveForAgent
	case req.Kind == KindOperator && req.OwnerUserID <= 0:
		return ErrOwnerRequired
	case req.Kind == KindAgent && req.OwnerUserID != 0:
		return invalid("owner_user_id is only for operator tokens")
	case req.Kind == KindAgent && !principalIDPattern.MatchString(req.PrincipalID):
		return ErrPrincipalRequired
	case req.Kind == KindOperator && req.PrincipalID != "":
		return invalid("principal_id is only for agent tokens")
	}
	return nil
}

func normalizeDatabases(databases []string) ([]string, error) {
	if len(databases) == 0 {
		return nil, invalid("databases must name at least one database or \"*\"")
	}
	seen := map[string]bool{}
	for _, name := range databases {
		if name == allDatabases {
			if len(databases) != 1 {
				return nil, invalid("databases: \"*\" must be the only entry")
			}
			return []string{allDatabases}, nil
		}
		if name == "" || len(name) > maxDatabaseLen || !utf8.ValidString(name) ||
			hasControl(name) {
			return nil, invalid("database name %q must be 1 to %d bytes without "+
				"control characters", name, maxDatabaseLen)
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
