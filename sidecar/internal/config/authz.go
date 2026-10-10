package config

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Unmapped-user policies for oauth.unmapped_users (E1). They apply only
// when oauth.role_mapping is non-empty.
const (
	// UnmappedUsersDeny refuses SSO users who are in no mapped group. It is
	// the default: an operator who lists groups is naming who may sign in,
	// and an IdP tenant usually holds far more people than that.
	UnmappedUsersDeny = "deny"
	// UnmappedUsersDefaultRole gives such users oauth.default_role.
	UnmappedUsersDefaultRole = "default_role"
)

// OAuthRoleMapping maps one IdP group to a pg_sage role.
type OAuthRoleMapping struct {
	Group string `yaml:"group" doc:"IdP group name exactly as the groups claim carries it (case-sensitive)."`
	Role  string `yaml:"role" doc:"pg_sage role for members of the group: admin, operator or viewer."`
}

// BreakGlassConfig is the local admin that works when the IdP is down.
type BreakGlassConfig struct {
	Enabled      bool   `yaml:"enabled" doc:"Enable the break-glass admin login (POST /api/v1/auth/break-glass). Every use is audited and alerts every channel."`
	PasswordHash string `yaml:"password_hash" doc:"bcrypt hash of the break-glass password. Prefer SAGE_BREAK_GLASS_PASSWORD_HASH or its _FILE variant." secret:"true"`
}

// validateAuthz checks the E1 settings: role mapping, the unmapped-user
// policy, break-glass and key rotation. Errors never echo secrets.
func (c *Config) validateAuthz() error {
	o := &c.OAuth
	if o.DefaultRole != "" && !isUserRole(o.DefaultRole) {
		return fmt.Errorf("oauth.default_role %q must be admin, operator or viewer",
			o.DefaultRole)
	}
	if strings.TrimSpace(o.GroupsClaim) == "" {
		return errors.New("oauth.groups_claim must not be empty")
	}
	if o.UnmappedUsers != UnmappedUsersDeny && o.UnmappedUsers != UnmappedUsersDefaultRole {
		return fmt.Errorf("oauth.unmapped_users %q must be %s or %s",
			o.UnmappedUsers, UnmappedUsersDeny, UnmappedUsersDefaultRole)
	}
	for i, m := range o.RoleMapping {
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("oauth.role_mapping[%d]: group must not be empty", i)
		}
		if !isUserRole(m.Role) {
			return fmt.Errorf("oauth.role_mapping[%d]: role %q must be admin, "+
				"operator or viewer", i, m.Role)
		}
	}
	if err := o.BreakGlass.validate(); err != nil {
		return err
	}
	if c.EncryptionKeyPrevious != "" && c.EncryptionKey == "" {
		return errors.New("encryption_key_previous requires encryption_key " +
			"(the key being rotated to)")
	}
	return nil
}

func (b *BreakGlassConfig) validate() error {
	if !b.Enabled {
		return nil
	}
	if b.PasswordHash == "" {
		return errors.New("oauth.break_glass.enabled requires password_hash " +
			"(or SAGE_BREAK_GLASS_PASSWORD_HASH[_FILE])")
	}
	if _, err := bcrypt.Cost([]byte(b.PasswordHash)); err != nil {
		return errors.New("oauth.break_glass.password_hash is not a bcrypt hash")
	}
	return nil
}

// isUserRole mirrors auth.ValidRoles; config cannot import auth.
func isUserRole(role string) bool {
	return role == "admin" || role == "operator" || role == "viewer"
}
