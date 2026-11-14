package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// ErrActionIdentity marks a refused action identity on a managed database
// (security-09b); the API answers it as invalid_action_identity.
var ErrActionIdentity = errors.New("invalid action identity")

// ActionCredentialInput sets or removes a meta-DB member's action identity:
// a second role on the member's own database that runs changes and backend
// signals. Only the role name and its password are stored; the identity
// always targets the member's host, port and database.
type ActionCredentialInput struct {
	// Username is the action role; "" removes the credential.
	Username string
	// Password is the action role's password; "" on an update keeps the
	// stored one.
	Password string
}

// actionPasswordAAD binds a sealed action password to its row and column,
// so a value copied to another row or to password_enc does not open.
func actionPasswordAAD(id int) string {
	return fmt.Sprintf("sage.databases|%d|action_password", id)
}

func actionInputError(msg string, wrapped ...error) error {
	err := fmt.Errorf("%w: %w: %s", ErrValidation, ErrActionIdentity, msg)
	for _, w := range wrapped {
		err = fmt.Errorf("%w (%w)", err, w)
	}
	return err
}

// validateActionInput checks the action fields of a create or update.
// Messages never carry the password.
func validateActionInput(in DatabaseInput, create bool) error {
	a := in.Action
	if a == nil {
		return nil
	}
	user := strings.TrimSpace(a.Username)
	switch {
	case a.Username != "" && user == "":
		return actionInputError("action_username is blank")
	case user == "" && a.Password != "":
		return actionInputError("action_password needs action_username")
	case user == "":
		return nil
	case create && a.Password == "":
		return actionInputError("action_password is required with action_username")
	case a.Username == in.Username:
		return actionInputError(fmt.Sprintf("role %q is the observation role; give the "+
			"action identity its own role", a.Username), config.ErrActionDSNTarget)
	}
	return nil
}

// sealActionPassword seals an action password for row id as sage-enc:v1.
func (s *DatabaseStore) sealActionPassword(id int, password string) ([]byte, error) {
	if s.keyring == nil {
		return nil, errNoPasswordKey
	}
	sealed, err := s.keyring.Seal(password, actionPasswordAAD(id))
	if err != nil {
		return nil, fmt.Errorf("encrypting action password: %w", err)
	}
	return []byte(sealed), nil
}

// actionWriteArgs returns the INSERT/UPDATE arguments of the action
// columns: set reports whether the input changes them, user is nil to
// clear, and enc is nil to keep the stored password.
func (s *DatabaseStore) actionWriteArgs(id int, a *ActionCredentialInput) (
	set bool, user *string, enc []byte, err error,
) {
	if a == nil {
		return false, nil, nil, nil
	}
	if a.Username == "" {
		return true, nil, nil, nil
	}
	if a.Password != "" {
		if enc, err = s.sealActionPassword(id, a.Password); err != nil {
			return false, nil, nil, err
		}
	}
	return true, &a.Username, enc, nil
}

// openActionPassword opens a stored action password. Only sage-enc:v1
// values under the row's action AAD open; anything else is
// ErrWrongEncryptionKey naming the database, logged once per database.
func (s *DatabaseStore) openActionPassword(id int, enc []byte) (string, error) {
	if s.keyring == nil || !crypto.IsSealed(string(enc)) {
		s.logActionWrongKeyOnce(id)
		return "", fmt.Errorf("database %d: action password: %w", id,
			crypto.ErrWrongEncryptionKey)
	}
	password, _, err := s.keyring.Open(string(enc), actionPasswordAAD(id))
	if err != nil {
		s.logActionWrongKeyOnce(id)
		return "", fmt.Errorf("database %d: action password: %w: %w", id,
			crypto.ErrWrongEncryptionKey, err)
	}
	return password, nil
}

// actionLogKey keeps the action password's wrong-key log apart from the
// observation password's in wrongKeyLogged.
type actionLogKey int

func (s *DatabaseStore) logActionWrongKeyOnce(id int) {
	if _, seen := s.wrongKeyLogged.LoadOrStore(actionLogKey(id), struct{}{}); seen {
		return
	}
	slog.Error("the stored action password does not open with encryption_key; the "+
		"database observes but every change fails until it is re-entered or the old "+
		"key is restored", "database_id", id)
}

// GetActionConnectionString returns the decrypted connection string of a
// database's action identity: the observation connection's host, port,
// database and sslmode with the action role. It is "" when none is set.
func (s *DatabaseStore) GetActionConnectionString(ctx context.Context, id int) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var host, dbName, sslmode string
	var port int
	var user *string
	var enc []byte
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT host, port, database_name, sslmode,
		        action_username, action_password_enc
		 FROM sage.databases WHERE id = $1`, id,
	).Scan(&host, &port, &dbName, &sslmode, &user, &enc)
	if err != nil {
		return "", fmt.Errorf("reading database %d action identity: %w", id, err)
	}
	if user == nil {
		return "", nil
	}
	password, err := s.openActionPassword(id, enc)
	if err != nil {
		return "", err
	}
	return databaseConnectionString(host, port, dbName, *user, password, sslmode), nil
}

// GetUpdateActionConnectionString builds the action identity an update
// would leave, without persisting it: "" when the update removes it or
// none is set. An empty input password reuses the stored one.
func (s *DatabaseStore) GetUpdateActionConnectionString(
	ctx context.Context, id int, input DatabaseInput,
) (string, error) {
	if err := validateActionInput(input, false); err != nil {
		return "", err
	}
	a := input.Action
	if a != nil && a.Username == "" {
		return "", nil
	}
	if a != nil && a.Password != "" {
		// A role with a new password needs nothing stored: re-entering it
		// is the remedy for a stored password no key opens.
		return databaseConnectionString(input.Host, input.Port, input.DatabaseName,
			a.Username, a.Password, input.SSLMode), nil
	}
	user, password, err := s.storedActionCredential(ctx, id)
	if err != nil {
		return "", err
	}
	if a != nil {
		if user == "" {
			return "", actionInputError("action_password is required: none is stored")
		}
		user = a.Username // a rename keeps the stored password
	}
	if user == "" {
		return "", nil
	}
	return databaseConnectionString(input.Host, input.Port, input.DatabaseName,
		user, password, input.SSLMode), nil
}

// storedActionCredential reads and opens the stored action role and
// password; both are "" when none is set.
func (s *DatabaseStore) storedActionCredential(ctx context.Context, id int) (
	string, string, error,
) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var user *string
	var enc []byte
	if err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT action_username,
		action_password_enc FROM sage.databases WHERE id = $1`, id,
	).Scan(&user, &enc); err != nil {
		return "", "", fmt.Errorf("reading database %d action identity: %w", id, err)
	}
	if user == nil {
		return "", "", nil
	}
	password, err := s.openActionPassword(id, enc)
	return *user, password, err
}

// actionConstraintError maps a refused action credential write to
// ErrActionIdentity; other errors pass through unchanged.
func actionConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		return err
	}
	switch pgErr.ConstraintName {
	case "databases_action_credential_pair":
		return actionInputError("action_password is required: none is stored")
	case "databases_action_role_distinct":
		return actionInputError("the observation username is the action role; give the "+
			"action identity its own role", config.ErrActionDSNTarget)
	}
	return err
}
