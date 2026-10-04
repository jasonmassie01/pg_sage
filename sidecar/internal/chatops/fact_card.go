package chatops

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fact cards (roadmap 2.3) reuse the approval-card token mechanism: a
// Confirm / Reject button carries the decision and an opaque 22-character
// token naming one fact card sent to one channel; the server keeps only
// the token's SHA-256. Who decided comes from the provider envelope.

// Slack button action ids of a fact card.
const (
	SlackFactConfirmAction = "sage_fact_confirm"
	SlackFactRejectAction  = "sage_fact_reject"
)

// factDataPrefix starts a fact card's Telegram callback data:
// "sage:fc:<token>" (confirm) or "sage:fr:<token>" (reject).
const factDataPrefix = "sage:f"

var slackFactActions = map[string]Decision{
	SlackFactConfirmAction: DecisionApprove,
	SlackFactRejectAction:  DecisionDeny,
}

// FactCallbackData is the Telegram button payload of a fact decision.
func FactCallbackData(d Decision, token string) string {
	verb := "r"
	if d == DecisionApprove {
		verb = "c"
	}
	return factDataPrefix + verb + ":" + token
}

// setFactData reads fact card callback data into the action.
func (a *Action) setFactData(data string) error {
	rest := strings.TrimPrefix(data, factDataPrefix)
	if len(rest) < 2 || rest[1] != ':' {
		return fmt.Errorf("%w: unknown fact callback data", ErrMalformed)
	}
	switch rest[0] {
	case 'c':
		a.Decision = DecisionApprove
	case 'r':
		a.Decision = DecisionDeny
	default:
		return fmt.Errorf("%w: unknown fact decision", ErrMalformed)
	}
	if token := rest[2:]; ValidCardToken(token) {
		a.FactToken = token
		return nil
	}
	return fmt.Errorf("%w: bad fact card token", ErrMalformed)
}

// FactCardIssue is one fact card about to be sent to one channel.
type FactCardIssue struct {
	ChannelID int
	Database  string
	FactID    int64
	FactHash  string
	Title     string
	ExpiresAt time.Time
}

// FactCardDelivery is one fact card sent to one channel.
type FactCardDelivery struct {
	ID        int64
	ChannelID int
	Database  string
	FactID    int64
	FactHash  string
	Title     string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedBy    *int
	Decision  string
	MessageID int64
}

// Expired reports whether the card can no longer be used at now.
func (d FactCardDelivery) Expired(now time.Time) bool { return !now.Before(d.ExpiresAt) }

// FactCardStore keeps fact-card deliveries in the notification control
// database.
type FactCardStore struct{ pool *pgxpool.Pool }

// NewFactCardStore returns a fact-card store on the control database.
func NewFactCardStore(pool *pgxpool.Pool) *FactCardStore { return &FactCardStore{pool: pool} }

func (in FactCardIssue) validate() error {
	switch {
	case in.ChannelID <= 0, in.FactID <= 0:
		return fmt.Errorf("%w: a fact card needs a channel and a fact", ErrMalformed)
	case strings.TrimSpace(in.Database) == "", strings.TrimSpace(in.FactHash) == "":
		return fmt.Errorf("%w: a fact card needs a database and a content hash",
			ErrMalformed)
	case !in.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: the fact card has already expired", ErrMalformed)
	}
	return nil
}

// Issue records a fact card for one channel and returns its new token.
func (s *FactCardStore) Issue(ctx context.Context, in FactCardIssue) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("chatops: fact card token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.pool.Exec(ctx, `/* pg_sage */ INSERT INTO sage.fact_card_deliveries
		(token_sha256, channel_id, database_name, fact_id, fact_hash, title, expires_at)
		VALUES ($1, $2, $3, $4, $5, left($6, 512), $7)`, tokenHash(token), in.ChannelID,
		in.Database, in.FactID, in.FactHash, in.Title, in.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("chatops: record fact card: %w", err)
	}
	return token, nil
}

const factCardColumns = `id, channel_id, database_name, fact_id, fact_hash, title,
	created_at, expires_at, used_at, used_by, COALESCE(decision, ''), message_id`

func scanFactCard(row pgx.Row) (FactCardDelivery, error) {
	var d FactCardDelivery
	err := row.Scan(&d.ID, &d.ChannelID, &d.Database, &d.FactID, &d.FactHash, &d.Title,
		&d.CreatedAt, &d.ExpiresAt, &d.UsedAt, &d.UsedBy, &d.Decision, &d.MessageID)
	return d, err
}

// Lookup returns the fact card a token names, used or not.
func (s *FactCardStore) Lookup(ctx context.Context, token string) (FactCardDelivery, error) {
	if !ValidCardToken(token) {
		return FactCardDelivery{}, ErrCardUnknown
	}
	d, err := scanFactCard(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+factCardColumns+`
		FROM sage.fact_card_deliveries WHERE token_sha256 = $1`, tokenHash(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return FactCardDelivery{}, ErrCardUnknown
	}
	if err != nil {
		return FactCardDelivery{}, fmt.Errorf("chatops: read fact card: %w", err)
	}
	return d, nil
}

// Consume uses a fact card once, recording who decided what from which
// chat message: a used card is ErrCardUsed, an expired one ErrCardExpired.
func (s *FactCardStore) Consume(ctx context.Context, token string, userID int, d Decision,
	messageID int64) (FactCardDelivery, error) {
	if !ValidCardToken(token) {
		return FactCardDelivery{}, ErrCardUnknown
	}
	decision := "reject"
	if d == DecisionApprove {
		decision = "confirm"
	}
	got, err := scanFactCard(s.pool.QueryRow(ctx, `/* pg_sage */ UPDATE
		sage.fact_card_deliveries SET used_at = now(), used_by = $2, decision = $3,
		message_id = $4 WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING `+factCardColumns, tokenHash(token), userID, decision, messageID))
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return FactCardDelivery{}, fmt.Errorf("chatops: use fact card: %w", err)
	}
	cur, lookupErr := s.Lookup(ctx, token)
	switch {
	case lookupErr != nil:
		return FactCardDelivery{}, lookupErr
	case cur.UsedAt != nil:
		return FactCardDelivery{}, ErrCardUsed
	default:
		return FactCardDelivery{}, ErrCardExpired
	}
}

// Revoke forgets a fact card whose message could not be delivered.
func (s *FactCardStore) Revoke(ctx context.Context, token string) error {
	if !ValidCardToken(token) {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `/* pg_sage */ DELETE FROM sage.fact_card_deliveries
		WHERE token_sha256 = $1 AND used_at IS NULL`, tokenHash(token)); err != nil {
		return fmt.Errorf("chatops: revoke fact card: %w", err)
	}
	return nil
}
