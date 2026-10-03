package chatops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrCardUnknown means no card has this token: forged, revoked or
	// malformed.
	ErrCardUnknown = errors.New("chatops: unknown approval card")
	// ErrCardExpired means the card's token expired before it was used.
	ErrCardExpired = errors.New("chatops: approval card expired")
	// ErrCardUsed means the card's token was already used.
	ErrCardUsed = errors.New("chatops: approval card already used")
)

// CardIssue is one approval card about to be sent to one channel.
type CardIssue struct {
	ChannelID int
	Database  string
	QueueID   int
	CardHash  string
	Title     string
	Summary   string
	ExpiresAt time.Time
}

// CardDelivery is one approval card sent to one channel.
type CardDelivery struct {
	ID              int64
	ChannelID       int
	Database        string
	QueueID         int
	CardHash        string
	Title           string
	Summary         string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	UsedAt          *time.Time
	UsedBy          *int
	Decision        string
	MessageID       int64
	FollowedUpAt    *time.Time
	FollowupVerdict string
}

// Expired reports whether the card can no longer be used at now.
func (d CardDelivery) Expired(now time.Time) bool { return !now.Before(d.ExpiresAt) }

// CardStore keeps approval-card deliveries in the notification control
// database. Tokens are stored only as their SHA-256.
type CardStore struct{ pool *pgxpool.Pool }

// NewCardStore returns a card store on the control database.
func NewCardStore(pool *pgxpool.Pool) *CardStore { return &CardStore{pool: pool} }

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (in CardIssue) validate() error {
	switch {
	case in.ChannelID <= 0, in.QueueID <= 0:
		return fmt.Errorf("%w: a card needs a channel and a queue item", ErrMalformed)
	case strings.TrimSpace(in.Database) == "", strings.TrimSpace(in.CardHash) == "":
		return fmt.Errorf("%w: a card needs a database and a content hash", ErrMalformed)
	case !in.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: the card has already expired", ErrMalformed)
	}
	return nil
}

// Issue records a card for one channel and returns its new token.
func (s *CardStore) Issue(ctx context.Context, in CardIssue) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("chatops: card token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.pool.Exec(ctx, `/* pg_sage */ INSERT INTO sage.approval_card_deliveries
		(token_sha256, channel_id, database_name, queue_id, card_hash, title, summary,
		 expires_at) VALUES ($1, $2, $3, $4, $5, left($6, 512), left($7, 512), $8)`,
		tokenHash(token), in.ChannelID, in.Database, in.QueueID, in.CardHash, in.Title,
		in.Summary, in.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("chatops: record approval card: %w", err)
	}
	return token, nil
}

const cardColumns = `id, channel_id, database_name, queue_id, card_hash, title, summary,
	created_at, expires_at, used_at, used_by, COALESCE(decision, ''), message_id,
	followed_up_at, followup_verdict`

func scanCard(row pgx.Row) (CardDelivery, error) {
	var d CardDelivery
	err := row.Scan(&d.ID, &d.ChannelID, &d.Database, &d.QueueID, &d.CardHash, &d.Title,
		&d.Summary, &d.CreatedAt, &d.ExpiresAt, &d.UsedAt, &d.UsedBy, &d.Decision,
		&d.MessageID, &d.FollowedUpAt, &d.FollowupVerdict)
	return d, err
}

// Lookup returns the card a token names, used or not.
func (s *CardStore) Lookup(ctx context.Context, token string) (CardDelivery, error) {
	if !ValidCardToken(token) {
		return CardDelivery{}, ErrCardUnknown
	}
	d, err := scanCard(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+cardColumns+`
		FROM sage.approval_card_deliveries WHERE token_sha256 = $1`, tokenHash(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return CardDelivery{}, ErrCardUnknown
	}
	if err != nil {
		return CardDelivery{}, fmt.Errorf("chatops: read approval card: %w", err)
	}
	return d, nil
}

// Consume uses a card once: it records who decided what from which chat
// message. A used card is ErrCardUsed, an expired one ErrCardExpired.
func (s *CardStore) Consume(ctx context.Context, token string, userID int, d Decision,
	messageID int64) (CardDelivery, error) {
	if !ValidCardToken(token) {
		return CardDelivery{}, ErrCardUnknown
	}
	got, err := scanCard(s.pool.QueryRow(ctx, `/* pg_sage */ UPDATE
		sage.approval_card_deliveries SET used_at = now(), used_by = $2, decision = $3,
		message_id = $4 WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING `+cardColumns, tokenHash(token), userID, string(d), messageID))
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CardDelivery{}, fmt.Errorf("chatops: use approval card: %w", err)
	}
	cur, lookupErr := s.Lookup(ctx, token)
	switch {
	case lookupErr != nil:
		return CardDelivery{}, lookupErr
	case cur.UsedAt != nil:
		return CardDelivery{}, ErrCardUsed
	default:
		return CardDelivery{}, ErrCardExpired
	}
}

// Revoke forgets a card whose message could not be delivered.
func (s *CardStore) Revoke(ctx context.Context, token string) error {
	if !ValidCardToken(token) {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `/* pg_sage */ DELETE FROM sage.approval_card_deliveries
		WHERE token_sha256 = $1 AND used_at IS NULL`, tokenHash(token)); err != nil {
		return fmt.Errorf("chatops: revoke approval card: %w", err)
	}
	return nil
}

// PendingFollowups returns up to limit cards whose outcome was not posted
// yet, oldest first.
func (s *CardStore) PendingFollowups(ctx context.Context, limit int) ([]CardDelivery,
	error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+cardColumns+`
		FROM sage.approval_card_deliveries WHERE followed_up_at IS NULL
		ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("chatops: list approval follow-ups: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CardDelivery, error) {
		return scanCard(r)
	})
	if err != nil {
		return nil, fmt.Errorf("chatops: list approval follow-ups: %w", err)
	}
	return out, nil
}

// MarkFollowedUp records that a card's outcome was posted (or closed with
// verdict); a card is followed up once.
func (s *CardStore) MarkFollowedUp(ctx context.Context, id int64, verdict string) error {
	tag, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.approval_card_deliveries
		SET followed_up_at = now(), followup_verdict = left($2, 64)
		WHERE id = $1 AND followed_up_at IS NULL`, id, verdict)
	if err != nil {
		return fmt.Errorf("chatops: mark approval follow-up: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCardUnknown
	}
	return nil
}
