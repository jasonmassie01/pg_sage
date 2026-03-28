package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
}

// DatabaseStore handles CRUD for sage.databases.
type DatabaseStore struct {
	pool       *pgxpool.Pool
	encryptKey []byte
}

// NewDatabaseStore creates a DatabaseStore with the given pool and
// 32-byte encryption key.
func NewDatabaseStore(pool *pgxpool.Pool, encryptKey []byte) *DatabaseStore {
	return &DatabaseStore{pool: pool, encryptKey: encryptKey}
}

// Create inserts a new database record. Returns the ID.
func (s *DatabaseStore) Create(
	ctx context.Context, input DatabaseInput, createdBy int,
) (int, error) {
	if err := validateInput(input, true); err != nil {
		return 0, err
	}

	count, err := s.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("checking database count: %w", err)
	}
	if count >= 50 {
		return 0, fmt.Errorf("maximum of 50 databases reached")
	}

	enc, err := crypto.Encrypt(input.Password, s.encryptKey)
	if err != nil {
		return 0, fmt.Errorf("encrypting password: %w", err)
	}

	tagsJSON, err := json.Marshal(input.Tags)
	if err != nil {
		return 0, fmt.Errorf("marshalling tags: %w", err)
	}

	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var id int
	err = s.pool.QueryRow(qctx,
		`INSERT INTO sage.databases
		    (name, host, port, database_name, username, password_enc,
		     sslmode, max_connections, tags, trust_level,
		     execution_mode, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING id`,
		input.Name, input.Host, input.Port, input.DatabaseName,
		input.Username, enc, input.SSLMode, input.MaxConnections,
		tagsJSON, input.TrustLevel, input.ExecutionMode, createdBy,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("inserting database: %w", err)
	}
	return id, nil
}

// List returns all database records (never returns password).
func (s *DatabaseStore) List(ctx context.Context) ([]DatabaseRecord, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`SELECT id, name, host, port, database_name, username,
		        sslmode, max_connections, enabled, tags, trust_level,
		        execution_mode, created_at, created_by, updated_at
		 FROM sage.databases ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	defer rows.Close()

	var results []DatabaseRecord
	for rows.Next() {
		var r DatabaseRecord
		var tagsJSON []byte
		var createdBy *int
		err := rows.Scan(
			&r.ID, &r.Name, &r.Host, &r.Port, &r.DatabaseName,
			&r.Username, &r.SSLMode, &r.MaxConnections, &r.Enabled,
			&tagsJSON, &r.TrustLevel, &r.ExecutionMode,
			&r.CreatedAt, &createdBy, &r.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning database row: %w", err)
		}
		if createdBy != nil {
			r.CreatedBy = *createdBy
		}
		r.Tags = make(map[string]string)
		if len(tagsJSON) > 0 {
			if jsonErr := json.Unmarshal(tagsJSON, &r.Tags); jsonErr != nil {
				return nil, fmt.Errorf("unmarshalling tags: %w", jsonErr)
			}
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// Get returns a single database record by ID.
func (s *DatabaseStore) Get(ctx context.Context, id int) (*DatabaseRecord, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var r DatabaseRecord
	var tagsJSON []byte
	var createdBy *int
	err := s.pool.QueryRow(qctx,
		`SELECT id, name, host, port, database_name, username,
		        sslmode, max_connections, enabled, tags, trust_level,
		        execution_mode, created_at, created_by, updated_at
		 FROM sage.databases WHERE id = $1`, id,
	).Scan(
		&r.ID, &r.Name, &r.Host, &r.Port, &r.DatabaseName,
		&r.Username, &r.SSLMode, &r.MaxConnections, &r.Enabled,
		&tagsJSON, &r.TrustLevel, &r.ExecutionMode,
		&r.CreatedAt, &createdBy, &r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("getting database %d: %w", id, err)
	}
	if createdBy != nil {
		r.CreatedBy = *createdBy
	}
	r.Tags = make(map[string]string)
	if len(tagsJSON) > 0 {
		if jsonErr := json.Unmarshal(tagsJSON, &r.Tags); jsonErr != nil {
			return nil, fmt.Errorf("unmarshalling tags: %w", jsonErr)
		}
	}
	return &r, nil
}

// Update modifies an existing database record.
// Password is optional (empty = no change).
func (s *DatabaseStore) Update(
	ctx context.Context, id int, input DatabaseInput,
) error {
	if err := validateInput(input, false); err != nil {
		return err
	}

	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tagsJSON, err := json.Marshal(input.Tags)
	if err != nil {
		return fmt.Errorf("marshalling tags: %w", err)
	}

	if input.Password != "" {
		enc, encErr := crypto.Encrypt(input.Password, s.encryptKey)
		if encErr != nil {
			return fmt.Errorf("encrypting password: %w", encErr)
		}
		_, err = s.pool.Exec(qctx,
			`UPDATE sage.databases SET
			    name=$1, host=$2, port=$3, database_name=$4,
			    username=$5, password_enc=$6, sslmode=$7,
			    max_connections=$8, tags=$9, trust_level=$10,
			    execution_mode=$11, updated_at=now()
			 WHERE id=$12`,
			input.Name, input.Host, input.Port, input.DatabaseName,
			input.Username, enc, input.SSLMode,
			input.MaxConnections, tagsJSON, input.TrustLevel,
			input.ExecutionMode, id,
		)
	} else {
		_, err = s.pool.Exec(qctx,
			`UPDATE sage.databases SET
			    name=$1, host=$2, port=$3, database_name=$4,
			    username=$5, sslmode=$6, max_connections=$7,
			    tags=$8, trust_level=$9, execution_mode=$10,
			    updated_at=now()
			 WHERE id=$11`,
			input.Name, input.Host, input.Port, input.DatabaseName,
			input.Username, input.SSLMode,
			input.MaxConnections, tagsJSON, input.TrustLevel,
			input.ExecutionMode, id,
		)
	}
	if err != nil {
		return fmt.Errorf("updating database %d: %w", id, err)
	}
	return nil
}

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
		`SELECT host, port, database_name, username,
		        password_enc, sslmode
		 FROM sage.databases WHERE id = $1`, id,
	).Scan(&host, &port, &dbName, &user, &enc, &sslmode)
	if err != nil {
		return "", fmt.Errorf("reading database %d: %w", id, err)
	}

	password, err := crypto.Decrypt(enc, s.encryptKey)
	if err != nil {
		return "", fmt.Errorf("decrypting password: %w", err)
	}

	connStr := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		user, password, host, port, dbName, sslmode,
	)
	return connStr, nil
}

// Count returns the number of databases.
func (s *DatabaseStore) Count(ctx context.Context) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var n int
	err := s.pool.QueryRow(qctx,
		"SELECT COUNT(*) FROM sage.databases",
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting databases: %w", err)
	}
	return n, nil
}
