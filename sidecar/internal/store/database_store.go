package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// DatabaseRecord represents a row from sage.databases.
// Password is never populated on read operations.
type DatabaseRecord struct {
	ID             int
	Name           string
	Host           string
	Port           int
	DatabaseName   string
	Username       string
	SSLMode        string
	MaxConnections int
	Enabled        bool
	Tags           map[string]string
	TrustLevel     string
	ExecutionMode  string
	CreatedAt      time.Time
	CreatedBy      int
	UpdatedAt      time.Time
	// Discovery (market-06): empty and nil for manual and CSV members.
	DiscoveredSource string
	Owner            string
	ExpiresAt        *time.Time
	AdoptedAt        *time.Time
	// ActionUsername is the action identity's role (security-09b); "" when
	// the member runs as one identity. Its password is never read here.
	ActionUsername string
	// Tier is full or observe_only (fleetpol-01d); only an enabled full
	// member gets a runtime (RunsRuntime).
	Tier string
}

// DatabaseInput holds user-provided fields for create/update.
type DatabaseInput struct {
	Name           string
	Host           string
	Port           int
	DatabaseName   string
	Username       string
	Password       string // plaintext, encrypted before storage
	SSLMode        string
	MaxConnections int
	Tags           map[string]string
	TrustLevel     string
	ExecutionMode  string
	// Action sets or removes the action identity (security-09b); nil
	// creates none and leaves it unchanged on update.
	Action *ActionCredentialInput
}

// DatabaseStore handles CRUD for sage.databases.
type DatabaseStore struct {
	pool       *pgxpool.Pool
	encryptKey []byte
	// keyring seals passwords as sage-enc:v1 under encryptKey and opens
	// values sealed under a previous key during a rotation (security-08).
	keyring *crypto.Keyring
	// legacy holds the pre-v0.9 keys, derived once, for lazy migration of
	// old ciphertext. The zero value disables migration.
	legacy crypto.LegacyKeys
	// wrongKeyLogged records database ids whose wrong-key ERROR has been
	// logged, so a fleet listing or reconnect loop logs each id once.
	wrongKeyLogged sync.Map
}

// NewDatabaseStore creates a DatabaseStore with the given pool and
// 32-byte encryption key. Without a valid key, passwords cannot be stored.
func NewDatabaseStore(pool *pgxpool.Pool, encryptKey []byte) *DatabaseStore {
	s := &DatabaseStore{pool: pool, encryptKey: encryptKey}
	return s.WithPreviousKey(nil)
}

// WithPreviousKey lets reads and ResealDatabasePasswords open passwords
// sealed under previous (the encryption_key_previous of a rotation). An
// empty previous key opens nothing extra.
func (s *DatabaseStore) WithPreviousKey(previous []byte) *DatabaseStore {
	s.keyring = nil
	if len(s.encryptKey) == 32 {
		// NewKeyring only fails on a key that is not 32 bytes.
		if kr, err := crypto.NewKeyring(s.encryptKey, previous); err == nil {
			s.keyring = kr
		}
	}
	return s
}

// allocateSealed takes the next sage.databases id and seals password for it.
func (s *DatabaseStore) allocateSealed(ctx context.Context, password string) (int, []byte, error) {
	var id int
	if err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		nextval(pg_get_serial_sequence('sage.databases', 'id'))::int`,
	).Scan(&id); err != nil {
		return 0, nil, fmt.Errorf("allocating database id: %w", err)
	}
	enc, err := s.sealPassword(id, password)
	return id, enc, err
}

// errNoPasswordKey refuses to store a password nothing can encrypt.
var errNoPasswordKey = fmt.Errorf("%w: database passwords need an encryption key: "+
	"set encryption_key, or data_dir so pg_sage can generate one", ErrValidation)

// sealPassword seals a password for row id as sage-enc:v1.
func (s *DatabaseStore) sealPassword(id int, password string) ([]byte, error) {
	if s.keyring == nil {
		return nil, errNoPasswordKey
	}
	sealed, err := s.keyring.Seal(password, dbPasswordAAD(id))
	if err != nil {
		return nil, fmt.Errorf("encrypting password: %w", err)
	}
	return []byte(sealed), nil
}

// dbPasswordAAD binds a sealed password to its row, so a password_enc
// copied to another row does not open there.
func dbPasswordAAD(id int) string {
	return fmt.Sprintf("sage.databases|%d|password", id)
}

// WithKeyMigration lets reads decrypt passwords stored by v0.8.x key
// derivations (SHA-256 and deterministic-salt argon2id) and re-encrypt
// them under the current key. passphrase must be the one that derived
// encryptKey. The legacy keys are derived here, once, never per read; an
// empty passphrase leaves migration off.
func (s *DatabaseStore) WithKeyMigration(passphrase string) *DatabaseStore {
	s.legacy = crypto.DeriveLegacyKeys(passphrase)
	return s
}

// Create inserts a new database record. Returns the ID.
func (s *DatabaseStore) Create(
	ctx context.Context, input DatabaseInput, createdBy int,
) (int, error) {
	return s.insert(ctx, input, createdBy, discoveryColumns{})
}

// discoveryColumns are the market-06 columns of a discovered member; the
// zero value (all NULL) is a manual or CSV member.
type discoveryColumns struct {
	source, owner *string
	expiresAt     *time.Time
	tier          string // "" is full (fleetpol-01d)
}

func (s *DatabaseStore) insert(
	ctx context.Context, input DatabaseInput, createdBy int, disc discoveryColumns,
) (int, error) {
	if err := s.checkCreate(input); err != nil {
		return 0, err
	}
	tagsJSON, err := marshalTags(input.Tags)
	if err != nil {
		return 0, err
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.insertRow(qctx, input, createdBy, disc, tagsJSON)
}

// checkCreate refuses an invalid input and a store with no key before a
// create allocates an id; insertRow checks the tier's cap under its lock.
func (s *DatabaseStore) checkCreate(input DatabaseInput) error {
	if err := validateInput(input, true); err != nil {
		return err
	}
	if err := validateActionInput(input, true); err != nil {
		return err
	}
	if s.keyring == nil {
		return errNoPasswordKey
	}
	return nil
}

// List returns all database records (never returns password).
func (s *DatabaseStore) List(ctx context.Context) ([]DatabaseRecord, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`/* pg_sage */ SELECT `+databaseColumns+` FROM sage.databases ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	defer rows.Close()

	var results []DatabaseRecord
	for rows.Next() {
		r, err := scanDatabaseRecord(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// Get returns a single database record by ID.
func (s *DatabaseStore) Get(ctx context.Context, id int) (*DatabaseRecord, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	r, err := scanDatabaseRecord(s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT `+databaseColumns+` FROM sage.databases WHERE id = $1`, id))
	if err != nil {
		return nil, fmt.Errorf("getting database %d: %w", id, err)
	}
	return &r, nil
}

// databaseColumns are the sage.databases columns scanDatabaseRecord reads.
const databaseColumns = `id, name, host, port, database_name, username,
	sslmode, max_connections, enabled, tags, trust_level, execution_mode,
	created_at, created_by, updated_at, COALESCE(discovered_source, ''),
	COALESCE(owner, ''), expires_at, adopted_at, COALESCE(action_username, ''), tier`

// scanDatabaseRecord scans one row of databaseColumns.
func scanDatabaseRecord(row pgx.Row) (DatabaseRecord, error) {
	var r DatabaseRecord
	var tagsJSON []byte
	var createdBy *int
	err := row.Scan(
		&r.ID, &r.Name, &r.Host, &r.Port, &r.DatabaseName,
		&r.Username, &r.SSLMode, &r.MaxConnections, &r.Enabled,
		&tagsJSON, &r.TrustLevel, &r.ExecutionMode,
		&r.CreatedAt, &createdBy, &r.UpdatedAt,
		&r.DiscoveredSource, &r.Owner, &r.ExpiresAt, &r.AdoptedAt, &r.ActionUsername,
		&r.Tier,
	)
	if err != nil {
		return r, fmt.Errorf("scanning database row: %w", err)
	}
	if createdBy != nil {
		r.CreatedBy = *createdBy
	}
	r.Tags = make(map[string]string)
	if len(tagsJSON) > 0 {
		if jsonErr := json.Unmarshal(tagsJSON, &r.Tags); jsonErr != nil {
			return r, fmt.Errorf("unmarshalling tags: %w", jsonErr)
		}
	}
	return r, nil
}

// Update modifies an existing database record. Password is optional
// (empty = no change); Action nil leaves the action identity unchanged.
func (s *DatabaseStore) Update(
	ctx context.Context, id int, input DatabaseInput,
) error {
	if err := validateInput(input, false); err != nil {
		return err
	}
	if err := validateActionInput(input, false); err != nil {
		return err
	}
	tagsJSON, err := json.Marshal(input.Tags)
	if err != nil {
		return fmt.Errorf("marshalling tags: %w", err)
	}
	var enc []byte
	if input.Password != "" {
		if enc, err = s.sealPassword(id, input.Password); err != nil {
			return err
		}
	}
	setAction, actUser, actEnc, err := s.actionWriteArgs(id, input.Action)
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, updateDatabaseSQL,
		input.Name, input.Host, input.Port, input.DatabaseName,
		input.Username, enc, input.SSLMode,
		input.MaxConnections, tagsJSON, id, setAction, actUser, actEnc,
	)
	if err != nil {
		return fmt.Errorf("updating database %d: %w", id, actionConstraintError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: database %d", ErrNotFound, id)
	}
	return nil
}

// updateDatabaseSQL writes a member's connection fields. A NULL $6 keeps
// the stored password. With $11 the action identity is set ($12 the role,
// $13 its sealed password, NULL to keep the stored one) or, with a NULL
// $12, removed; the table's checks refuse a role with no password.
const updateDatabaseSQL = `/* pg_sage */ UPDATE sage.databases SET
	    name=$1, host=$2, port=$3, database_name=$4, username=$5,
	    password_enc=COALESCE($6::bytea, password_enc), sslmode=$7,
	    max_connections=$8, tags=$9, updated_at=now(),
	    action_username = CASE WHEN $11::boolean THEN $12::text
	                           ELSE action_username END,
	    action_password_enc = CASE WHEN NOT $11::boolean THEN action_password_enc
	                               WHEN $12::text IS NULL THEN NULL
	                               ELSE COALESCE($13::bytea, action_password_enc) END
	 WHERE id=$10`

// Delete removes a database record.
func (s *DatabaseStore) Delete(ctx context.Context, id int) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := s.pool.Exec(qctx,
		"DELETE FROM sage.databases WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("deleting database %d: %w", id, err)
	}
	return nil
}

// GetConnectionString returns the decrypted connection string
// for a database.
func (s *DatabaseStore) GetConnectionString(
	ctx context.Context, id int,
) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var host, dbName, user, sslmode string
	var port int
	var enc []byte
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT host, port, database_name, username,
		        password_enc, sslmode
		 FROM sage.databases WHERE id = $1`, id,
	).Scan(&host, &port, &dbName, &user, &enc, &sslmode)
	if err != nil {
		return "", fmt.Errorf("reading database %d: %w", id, err)
	}

	password, err := s.decryptPassword(ctx, id, enc)
	if err != nil {
		return "", err
	}

	return databaseConnectionString(
		host, port, dbName, user, password, sslmode,
	), nil
}

// GetUpdateConnectionString builds the candidate connection string for an
// update without persisting it. An empty input password reuses and decrypts
// the currently stored password.
func (s *DatabaseStore) GetUpdateConnectionString(
	ctx context.Context, id int, input DatabaseInput,
) (string, error) {
	if err := validateInput(input, false); err != nil {
		return "", err
	}
	password := input.Password
	if password == "" {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var enc []byte
		if err := s.pool.QueryRow(qctx,
			`/* pg_sage */ SELECT password_enc
			 FROM sage.databases WHERE id = $1`, id,
		).Scan(&enc); err != nil {
			return "", fmt.Errorf("reading database %d password: %w", id, err)
		}
		var err error
		password, err = s.decryptPassword(ctx, id, enc)
		if err != nil {
			return "", err
		}
	}
	return databaseConnectionString(
		input.Host, input.Port, input.DatabaseName,
		input.Username, password, input.SSLMode,
	), nil
}

func databaseConnectionString(
	host string, port int, databaseName, username, password, sslMode string,
) string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(username, password),
		Host:     fmt.Sprintf("%s:%d", host, port),
		Path:     databaseName,
		RawQuery: url.Values{"sslmode": {sslMode}}.Encode(),
	}
	return u.String()
}

// Count returns the number of full members, the ones the 50-member cap
// counts (observe-only members have their own, CountTier).
func (s *DatabaseStore) Count(ctx context.Context) (int, error) {
	n, err := s.CountTier(ctx, TierFull)
	if err != nil {
		return 0, fmt.Errorf("counting databases: %w", err)
	}
	return n, nil
}
