package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/auth"
)

// principals/users.yaml (cli-02b) maps code-host logins to pg_sage users,
// so a merge of an approval PR is an approval by a known person:
//
//	kind: CodeHostUsers
//	schema_version: 1
//	users:
//	  - login: octo-dba
//	    email: alice@example.com
//
// The file is reviewed in git like the rest of policy.directory. A login
// approves only as an active user with the operator role (or admin) on
// the change's database, per-database grants included.

const (
	// UsersFile is the login map's path under policy.directory.
	UsersFile          = DirPrincipals + "/users.yaml"
	kindCodeHostUsers  = "CodeHostUsers"
	usersFileAltSuffix = "users.yml"
)

// isUsersFile reports the login map's file name inside principals/.
func isUsersFile(name string) bool {
	lower := strings.ToLower(name)
	return lower == "users.yaml" || lower == usersFileAltSuffix
}

type usersFile struct {
	Kind          string `yaml:"kind"`
	SchemaVersion int    `yaml:"schema_version"`
	Users         []struct {
		Login string `yaml:"login"`
		Email string `yaml:"email"`
	} `yaml:"users"`
}

// LoadCodeHostUsers reads root's principals/users.yaml as login (lower
// case) to email (lower case). A missing file or root maps nobody
// (ErrUnmappedLogin); an invalid file is an error naming the file.
func LoadCodeHostUsers(root string) (map[string]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: policy.directory is not set", ErrUnmappedLogin)
	}
	body, err := readBounded(filepath.Join(root, filepath.FromSlash(UsersFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrUnmappedLogin, UsersFile)
	}
	if err != nil {
		return nil, fmt.Errorf("gitops: reading %s: %w", UsersFile, err)
	}
	var f usersFile
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("gitops: %s: %w", UsersFile, err)
	}
	if f.Kind != kindCodeHostUsers || f.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("gitops: %s must have kind %s and schema_version %d",
			UsersFile, kindCodeHostUsers, SchemaVersion)
	}
	out := make(map[string]string, len(f.Users))
	for i, u := range f.Users {
		login := strings.ToLower(strings.TrimSpace(u.Login))
		email := auth.NormalizeEmail(u.Email)
		if login == "" || email == "" {
			return nil, fmt.Errorf("gitops: %s: users[%d] needs a login and email", UsersFile,
				i)
		}
		if _, dup := out[login]; dup {
			return nil, fmt.Errorf("gitops: %s: login %s is listed twice", UsersFile, login)
		}
		out[login] = email
	}
	return out, nil
}

// DirectoryUsers resolves logins through root's users.yaml (read on each
// call) and the users and grants in the control database.
type DirectoryUsers struct {
	Pool *pgxpool.Pool
	Root string
}

// Resolve implements ApprovalUsers.
func (u *DirectoryUsers) Resolve(ctx context.Context, login, database string) (Approver,
	error) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return Approver{}, fmt.Errorf("%w: the merge has no login", ErrUnmappedLogin)
	}
	logins, err := LoadCodeHostUsers(u.Root)
	if err != nil {
		return Approver{}, err
	}
	email, ok := logins[login]
	if !ok {
		return Approver{}, fmt.Errorf("%w: %s", ErrUnmappedLogin, login)
	}
	if u.Pool == nil {
		return Approver{}, errors.New("gitops: no control database to resolve users")
	}
	a, err := activeUser(ctx, u.Pool, email)
	if err != nil {
		return Approver{}, err
	}
	grants, err := auth.NewDatabaseGrantStore(u.Pool).UserDatabaseGrants(ctx, a.UserID)
	if err != nil {
		return Approver{}, fmt.Errorf("gitops: reading the grants of %s: %w", email, err)
	}
	role := auth.EffectiveDatabaseRole(a.Role, grants, database)
	if !auth.RoleAtLeast(role, auth.RoleOperator) {
		return a, fmt.Errorf("%w: %s is %q on %s", ErrNotOperator, email, role, database)
	}
	return a, nil
}

// activeUser is the one enabled user with email (any case).
func activeUser(ctx context.Context, pool *pgxpool.Pool, email string) (Approver, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := pool.Query(qctx, `/* pg_sage */ SELECT id, email, role FROM sage.users
		WHERE lower(email) = lower($1) AND disabled_at IS NULL ORDER BY id LIMIT 2`, email)
	if err != nil {
		return Approver{}, fmt.Errorf("gitops: looking up %s: %w", email, err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approver, error) {
		var a Approver
		return a, r.Scan(&a.UserID, &a.Email, &a.Role)
	})
	if err != nil {
		return Approver{}, fmt.Errorf("gitops: looking up %s: %w", email, err)
	}
	if len(list) != 1 {
		return Approver{Email: email}, fmt.Errorf("%w: %s is not one active pg_sage user",
			ErrUnmappedLogin, email)
	}
	return list[0], nil
}
