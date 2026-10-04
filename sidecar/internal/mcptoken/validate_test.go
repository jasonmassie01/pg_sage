package mcptoken_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Pure request validation. A store without a pool must reject every
// invalid request before it touches the database, so these tests need no
// Postgres. Valid requests reach the (missing) database and fail with an
// error that is NOT a validation error (error propagation: callers can
// tell "your request is wrong" from "storage failed").

// No concurrent access tests here: validation is stateless; the store's
// concurrent behaviour is covered against Postgres in store_db_test.go.

const day = 24 * time.Hour

func validAgent() mcptoken.CreateRequest {
	return mcptoken.CreateRequest{
		Name:      "ci-agent",
		Kind:      mcptoken.KindAgent,
		Scopes:    []string{mcptoken.ScopeRead, mcptoken.ScopePropose},
		Databases: []string{"orders"},
		ExpiresIn: day,
		CreatedBy: "admin@example.com",
	}
}

func validOperator() mcptoken.CreateRequest {
	req := validAgent()
	req.Kind = mcptoken.KindOperator
	req.Scopes = []string{mcptoken.ScopeRead, mcptoken.ScopePropose, mcptoken.ScopeApprove}
	req.OwnerUserID = 7
	return req
}

type invalidCase struct {
	name   string
	base   func() mcptoken.CreateRequest
	mutate func(*mcptoken.CreateRequest)
	want   error
	field  string // substring the wrapped ErrInvalid detail must mention
}

var nameCases = []invalidCase{
	{"empty name", validAgent, func(r *mcptoken.CreateRequest) { r.Name = "" },
		mcptoken.ErrInvalid, "name"},
	{"101 char name", validAgent,
		func(r *mcptoken.CreateRequest) { r.Name = strings.Repeat("a", 101) },
		mcptoken.ErrInvalid, "name"},
	{"newline in name", validAgent, func(r *mcptoken.CreateRequest) { r.Name = "ci\nagent" },
		mcptoken.ErrInvalid, "name"},
	{"NUL in name", validAgent, func(r *mcptoken.CreateRequest) { r.Name = "ci\x00agent" },
		mcptoken.ErrInvalid, "name"},
	{"tab in name", validAgent, func(r *mcptoken.CreateRequest) { r.Name = "ci\tagent" },
		mcptoken.ErrInvalid, "name"},
	{"DEL in name", validAgent, func(r *mcptoken.CreateRequest) { r.Name = "ci\x7fagent" },
		mcptoken.ErrInvalid, "name"},
}

var scopeCases = []invalidCase{
	{"nil scopes", validAgent, func(r *mcptoken.CreateRequest) { r.Scopes = nil },
		mcptoken.ErrInvalid, "scope"},
	{"empty scopes", validAgent, func(r *mcptoken.CreateRequest) { r.Scopes = []string{} },
		mcptoken.ErrInvalid, "scope"},
	{"propose without read", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"propose"} },
		mcptoken.ErrInvalid, "scope"},
	{"unknown scope", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"read", "admin"} },
		mcptoken.ErrInvalid, "scope"},
	{"empty scope string", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"read", ""} },
		mcptoken.ErrInvalid, "scope"},
	{"upper-case scope", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"READ"} },
		mcptoken.ErrInvalid, "scope"},
}

var kindCases = []invalidCase{
	{"agent with approve", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"read", "propose", "approve"} },
		mcptoken.ErrApproveForAgent, ""},
	{"agent with read+approve", validAgent,
		func(r *mcptoken.CreateRequest) { r.Scopes = []string{"approve", "read"} },
		mcptoken.ErrApproveForAgent, ""},
	{"operator without owner", validOperator,
		func(r *mcptoken.CreateRequest) { r.OwnerUserID = 0 }, mcptoken.ErrOwnerRequired, ""},
	{"operator negative owner", validOperator,
		func(r *mcptoken.CreateRequest) { r.OwnerUserID = -1 }, mcptoken.ErrOwnerRequired, ""},
	{"agent with owner", validAgent,
		func(r *mcptoken.CreateRequest) { r.OwnerUserID = 3 }, mcptoken.ErrInvalid, "owner"},
	{"unknown kind", validAgent,
		func(r *mcptoken.CreateRequest) { r.Kind = "robot" }, mcptoken.ErrInvalid, "kind"},
	{"empty kind", validAgent,
		func(r *mcptoken.CreateRequest) { r.Kind = "" }, mcptoken.ErrInvalid, "kind"},
}

var databaseCases = []invalidCase{
	{"nil databases", validAgent, func(r *mcptoken.CreateRequest) { r.Databases = nil },
		mcptoken.ErrInvalid, "database"},
	{"empty databases", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{} },
		mcptoken.ErrInvalid, "database"},
	{"star then name", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"*", "orders"} },
		mcptoken.ErrInvalid, "database"},
	{"name then star", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"orders", "*"} },
		mcptoken.ErrInvalid, "database"},
	{"64 char database", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{strings.Repeat("d", 64)} },
		mcptoken.ErrInvalid, "database"},
	{"empty database name", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"orders", ""} },
		mcptoken.ErrInvalid, "database"},
	{"newline in database", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"or\nders"} },
		mcptoken.ErrInvalid, "database"},
	{"DEL in database", validAgent,
		func(r *mcptoken.CreateRequest) { r.Databases = []string{"orders\x7f"} },
		mcptoken.ErrInvalid, "database"},
}

var lifetimeCases = []invalidCase{
	{"59m59s", validAgent,
		func(r *mcptoken.CreateRequest) { r.ExpiresIn = time.Hour - time.Second },
		mcptoken.ErrInvalid, "expir"},
	{"zero lifetime", validAgent, func(r *mcptoken.CreateRequest) { r.ExpiresIn = 0 },
		mcptoken.ErrInvalid, "expir"},
	{"negative lifetime", validAgent,
		func(r *mcptoken.CreateRequest) { r.ExpiresIn = -time.Hour },
		mcptoken.ErrInvalid, "expir"},
	{"90d plus 1s", validAgent,
		func(r *mcptoken.CreateRequest) { r.ExpiresIn = 90*day + time.Second },
		mcptoken.ErrInvalid, "expir"},
	{"empty created_by", validAgent, func(r *mcptoken.CreateRequest) { r.CreatedBy = "" },
		mcptoken.ErrInvalid, "created"},
}

func runInvalid(t *testing.T, cases []invalidCase) {
	t.Helper()
	store := mcptoken.NewStore(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.base()
			tc.mutate(&req)
			tok, err := store.Create(context.Background(), req)
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, "", tok.ID)
			require.Equal(t, "", tok.Secret)
			requireOnlySentinel(t, err, tc.want)
			if tc.field != "" {
				require.NotEqual(t, tc.want.Error(), err.Error(),
					"ErrInvalid must be wrapped with detail")
				require.ErrorContains(t, err, tc.field)
			}
		})
	}
}

// requireOnlySentinel keeps the three refusal classes distinguishable.
func requireOnlySentinel(t *testing.T, err, want error) {
	t.Helper()
	for _, other := range []error{mcptoken.ErrApproveForAgent, mcptoken.ErrOwnerRequired,
		mcptoken.ErrNotFound, mcptoken.ErrUnauthorized} {
		if other == want {
			continue
		}
		require.False(t, errors.Is(err, other), "%v must not match %v", err, other)
	}
}

func TestCreateRejectsInvalidName(t *testing.T)      { runInvalid(t, nameCases) }
func TestCreateRejectsInvalidScopes(t *testing.T)    { runInvalid(t, scopeCases) }
func TestCreateRejectsKindViolations(t *testing.T)   { runInvalid(t, kindCases) }
func TestCreateRejectsInvalidDatabases(t *testing.T) { runInvalid(t, databaseCases) }
func TestCreateRejectsInvalidLifetime(t *testing.T)  { runInvalid(t, lifetimeCases) }

// requireReachesStorage: a valid request passes validation, so with no
// pool the store fails for a storage reason, never a validation one.
func requireReachesStorage(t *testing.T, req mcptoken.CreateRequest) {
	t.Helper()
	tok, err := mcptoken.NewStore(nil).Create(context.Background(), req)
	require.Error(t, err)
	require.Equal(t, "", tok.Secret)
	for _, sentinel := range []error{mcptoken.ErrInvalid, mcptoken.ErrApproveForAgent,
		mcptoken.ErrOwnerRequired, mcptoken.ErrUnauthorized} {
		require.False(t, errors.Is(err, sentinel),
			"valid request must not fail validation: %v", err)
	}
}

type validCase struct {
	name   string
	mutate func(*mcptoken.CreateRequest)
}

var validCases = []validCase{
	{"1 char name", func(r *mcptoken.CreateRequest) { r.Name = "a" }},
	{"100 char name", func(r *mcptoken.CreateRequest) { r.Name = strings.Repeat("a", 100) }},
	{"name with inner space", func(r *mcptoken.CreateRequest) { r.Name = "ci agent 2" }},
	{"read only", func(r *mcptoken.CreateRequest) { r.Scopes = []string{"read"} }},
	{"duplicate scopes", dupScopes},
	{"unsorted scopes", func(r *mcptoken.CreateRequest) {
		r.Scopes = []string{"propose", "read"}
	}},
	{"star database", func(r *mcptoken.CreateRequest) { r.Databases = []string{"*"} }},
	{"63 char database", func(r *mcptoken.CreateRequest) {
		r.Databases = []string{strings.Repeat("d", 63)}
	}},
	{"duplicate databases", func(r *mcptoken.CreateRequest) {
		r.Databases = []string{"b", "a", "b"}
	}},
	{"exactly MinLifetime", func(r *mcptoken.CreateRequest) { r.ExpiresIn = time.Hour }},
	{"exactly MaxLifetime", func(r *mcptoken.CreateRequest) { r.ExpiresIn = 90 * day }},
	{"unicode name and db", unicodeNames},
}

func TestCreateValidBoundariesPassValidation(t *testing.T) {
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			req := validAgent()
			tc.mutate(&req)
			requireReachesStorage(t, req)
		})
	}
}

func dupScopes(r *mcptoken.CreateRequest) { r.Scopes = []string{"read", "read", "propose"} }

func unicodeNames(r *mcptoken.CreateRequest) {
	r.Name = "agent-é"
	r.Databases = []string{"bestellungen_ü"}
}

func TestCreateOperatorShapePassesValidation(t *testing.T) {
	// The owner's role can only be checked in the database, so a well-formed
	// operator request reaches storage.
	requireReachesStorage(t, validOperator())
	req := validOperator()
	req.Scopes = []string{"read"}
	requireReachesStorage(t, req)
}

func TestLifetimeConstants(t *testing.T) {
	require.Equal(t, time.Hour, mcptoken.MinLifetime)
	require.Equal(t, 90*day, mcptoken.MaxLifetime)
	require.Equal(t, "pgs_mcp_", mcptoken.SecretPrefix)
	require.Equal(t, mcptoken.Kind("agent"), mcptoken.KindAgent)
	require.Equal(t, mcptoken.Kind("operator"), mcptoken.KindOperator)
}

func TestHashSecretIsSHA256Base64URL(t *testing.T) {
	secret := mcptoken.SecretPrefix + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	sum := sha256.Sum256([]byte(secret))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	got := mcptoken.HashSecret(secret)
	require.Equal(t, want, got)
	require.Equal(t, got, mcptoken.HashSecret(secret), "must be deterministic")
	require.Len(t, got, 43)
	require.NotContains(t, got, "=")
	require.NotContains(t, got, "+")
	require.NotContains(t, got, "/")
	require.NotContains(t, got, secret)
	require.NotContains(t, got, "abcdefghijklmnop")
}

func TestHashSecretDiffersPerSecret(t *testing.T) {
	a := mcptoken.HashSecret("pgs_mcp_aaaaaaaa")
	b := mcptoken.HashSecret("pgs_mcp_aaaaaaab")
	require.NotEqual(t, a, b)
	empty := mcptoken.HashSecret("")
	require.Len(t, empty, 43)
	require.NotEqual(t, a, empty)
}

func TestLooksLikeToken(t *testing.T) {
	cases := map[string]bool{
		"pgs_mcp_abcdef":         true,
		mcptoken.SecretPrefix:    true,
		"":                       false,
		"pgs_mcp":                false,
		"PGS_MCP_abcdef":         false,
		" pgs_mcp_abcdef":        false,
		"Bearer pgs_mcp_abcdef":  false,
		"pgs_agent_abcdef":       false,
		"0f3c1e5a-session-value": false,
	}
	for in, want := range cases {
		require.Equal(t, want, mcptoken.LooksLikeToken(in), "LooksLikeToken(%q)", in)
	}
}
