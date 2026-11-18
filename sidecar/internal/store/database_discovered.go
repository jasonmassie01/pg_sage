package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDiscoveredTrust rejects a trust_level or execution_mode change on a
// discovered member no owner has adopted (market-06). Discovery enrols at
// observation with approval; only adoption hands the member to the normal
// trust paths.
var ErrDiscoveredTrust = errors.New("discovered database: an owner must adopt it " +
	"before its trust level or execution mode changes")

// discoveredTrustSQLState is raised by sage.databases_discovered_trust_guard.
const discoveredTrustSQLState = "SGD01"

// DiscoveredTrustError maps the guard trigger's error to ErrDiscoveredTrust;
// any other error is returned unchanged.
func DiscoveredTrustError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == discoveredTrustSQLState {
		return fmt.Errorf("%w: %s", ErrDiscoveredTrust, pgErr.Message)
	}
	return err
}

// discoveredMaxConnections bounds the connection growth of each enrolled
// member: one observation connection.
const discoveredMaxConnections = 1

// DiscoveredMember is a database a discovery source found, enrolled with
// the credentials of the member it was found through.
type DiscoveredMember struct {
	Name         string
	DatabaseName string
	Source       string
	Owner        string
	ExpiresAt    time.Time
	// Tags are the source's tags for the database (fleetpol-01b), so the
	// policy set the member follows sees what enrollment matched on.
	// discovered_from is always the credential member's name.
	Tags map[string]string
	// Tier is the member's tier (fleetpol-01d): "" or full, or
	// observe_only for a set whose enrollment.tier says so.
	Tier string
}

func (m DiscoveredMember) validate() error {
	switch {
	case strings.TrimSpace(m.Source) == "":
		return fmt.Errorf("%w: a discovered member needs its source", ErrValidation)
	case strings.TrimSpace(m.Owner) == "":
		return fmt.Errorf("%w: a discovered member needs an owner", ErrValidation)
	case m.ExpiresAt.IsZero():
		return fmt.Errorf("%w: a discovered member needs expires_at", ErrValidation)
	case m.Tier != "" && !ValidTier(m.Tier):
		return fmt.Errorf("%w: a discovered member's tier %q must be %s or %s",
			ErrValidation, m.Tier, TierFull, TierObserveOnly)
	}
	return nil
}

// CreateDiscovered inserts a discovered member that reuses the role,
// password, host, port and sslmode of member fromID. It is always trust
// observation, execution approval and one connection; there is no
// parameter that changes that.
func (s *DatabaseStore) CreateDiscovered(
	ctx context.Context, fromID int, m DiscoveredMember,
) (int, error) {
	if err := m.validate(); err != nil {
		return 0, err
	}
	from, err := s.Get(ctx, fromID)
	if err != nil {
		return 0, fmt.Errorf("reading credential member %d: %w", fromID, err)
	}
	password, err := s.memberPassword(ctx, fromID)
	if err != nil {
		return 0, err
	}
	tags := make(map[string]string, len(m.Tags)+1)
	for k, v := range m.Tags {
		tags[k] = v
	}
	tags["discovered_from"] = from.Name
	input := DatabaseInput{Name: m.Name, Host: from.Host, Port: from.Port,
		DatabaseName: m.DatabaseName, Username: from.Username, Password: password,
		SSLMode: from.SSLMode, MaxConnections: discoveredMaxConnections,
		Tags: tags, TrustLevel: "observation", ExecutionMode: "approval"}
	expires := m.ExpiresAt
	id, err := s.insert(ctx, input, 0, discoveryColumns{
		source: &m.Source, owner: &m.Owner, expiresAt: &expires, tier: m.Tier})
	return id, DiscoveredTrustError(err)
}

// memberPassword opens member id's stored password.
func (s *DatabaseStore) memberPassword(ctx context.Context, id int) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var enc []byte
	if err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT password_enc FROM sage.databases WHERE id = $1`, id,
	).Scan(&enc); err != nil {
		return "", fmt.Errorf("reading database %d password: %w", id, err)
	}
	return s.decryptPassword(ctx, id, enc)
}

// DisableDiscovered disables a discovered member (expired or gone). A
// manual member is never touched.
func (s *DatabaseStore) DisableDiscovered(ctx context.Context, id int) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.databases SET enabled = false, updated_at = now()
		 WHERE id = $1 AND discovered_source IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("disabling discovered database %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: discovered database %d", ErrNotFound, id)
	}
	return nil
}
