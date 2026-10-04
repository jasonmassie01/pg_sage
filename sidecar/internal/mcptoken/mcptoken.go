// Package mcptoken issues, lists, revokes and validates the scoped API
// tokens MCP clients authenticate with (control database, table
// sage.mcp_tokens). Only the SHA-256 of a token is stored; the plaintext
// is returned once, by Create.
//
// Agent tokens can read and propose, never approve. Operator tokens are
// bound to a person (an operator or admin) and carry at most the scopes of
// that person's current role, re-checked on every use.
package mcptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SecretPrefix starts every plaintext token: SecretPrefix + 43 base64url
// characters (32 random bytes).
const SecretPrefix = "pgs_mcp_"

// Token lifetimes accepted by Create.
const (
	MinLifetime = time.Hour
	MaxLifetime = 90 * 24 * time.Hour
)

const (
	secretBytes   = 32
	secretBodyLen = 43 // base64url of 32 bytes, no padding
	prefixLen     = 12
)

// Kind is who a token is for.
type Kind string

const (
	KindAgent    Kind = "agent"
	KindOperator Kind = "operator"
)

// Scopes, in canonical order.
const (
	ScopeRead    = "read"
	ScopePropose = "propose"
	ScopeApprove = "approve"
)

var (
	ErrInvalid         = errors.New("mcptoken: invalid token request")
	ErrApproveForAgent = errors.New("mcptoken: approve scope is only for operator tokens")
	ErrOwnerRequired   = errors.New(
		"mcptoken: an operator token needs an operator or admin owner")
	ErrNotFound     = errors.New("mcptoken: token not found")
	ErrUnauthorized = errors.New("mcptoken: token invalid, expired or revoked")
)

// CreateRequest describes a token to issue.
type CreateRequest struct {
	Name        string
	Kind        Kind
	Scopes      []string
	Databases   []string // ["*"] = every database
	ExpiresIn   time.Duration
	OwnerUserID int // operator tokens only
	CreatedBy   string
}

// Token is a stored token. Secret is set only in Create's return value.
type Token struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        Kind       `json:"kind"`
	Scopes      []string   `json:"scopes"`
	Databases   []string   `json:"databases"` // ["*"] when all
	OwnerUserID *int       `json:"owner_user_id,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	RevokedBy   string     `json:"revoked_by,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	Prefix      string     `json:"prefix"`
	Secret      string     `json:"token,omitempty"`
}

// Grant is what a validated token may do right now.
type Grant struct {
	TokenID     string
	Name        string
	Kind        Kind
	Scopes      []string
	Databases   []string // nil = every database
	OwnerUserID int      // operator tokens only
	OwnerRole   string   // operator tokens only: the owner's current role
}

// HashSecret is the stored form of a secret: SHA-256, base64url without
// padding.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// LooksLikeToken reports whether s is shaped like an MCP token (has the
// prefix). It does not validate it.
func LooksLikeToken(s string) bool {
	return strings.HasPrefix(s, SecretPrefix)
}

func newSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mcptoken: generating secret: %w", err)
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// wellFormedSecret checks the exact shape so malformed input never
// reaches the database.
func wellFormedSecret(secret string) bool {
	if !LooksLikeToken(secret) || len(secret) != len(SecretPrefix)+secretBodyLen {
		return false
	}
	for _, c := range secret[len(SecretPrefix):] {
		if !isBase64URL(c) {
			return false
		}
	}
	return true
}

func isBase64URL(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_'
}

// roleScopes are the scopes a person's role allows.
func roleScopes(role string) map[string]bool {
	switch role {
	case "admin", "operator":
		return map[string]bool{ScopeRead: true, ScopePropose: true, ScopeApprove: true}
	case "viewer":
		return map[string]bool{ScopeRead: true}
	default:
		return nil
	}
}
