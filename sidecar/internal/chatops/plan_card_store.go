package chatops

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxPlanOrdinal is the last step ordinal a plan can have (plan-01a).
const maxPlanOrdinal = 50

var planHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PlanCardIssue is one plan card about to be sent to one channel: the plan
// and hash it shows, the decisions it offers and, with approve_up_to, the
// last step that button approves.
type PlanCardIssue struct {
	ChannelID int
	Database  string
	PlanID    int64
	PlanHash  string
	Title     string
	Offered   []PlanChoice
	UpTo      int
	ExpiresAt time.Time
}

// PlanCardDelivery is one plan card sent to one channel.
type PlanCardDelivery struct {
	ID        int64
	ChannelID int
	Database  string
	PlanID    int64
	PlanHash  string
	Title     string
	Offered   []PlanChoice
	UpTo      int
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedBy    *int
	Decision  PlanChoice
	MessageID int64
}

// Expired reports whether the card can no longer be used at now.
func (d PlanCardDelivery) Expired(now time.Time) bool { return !now.Before(d.ExpiresAt) }

// PlanCardStore keeps plan-card deliveries in the notification control
// database.
type PlanCardStore struct{ pool *pgxpool.Pool }

// NewPlanCardStore returns a plan-card store on the control database.
func NewPlanCardStore(pool *pgxpool.Pool) *PlanCardStore { return &PlanCardStore{pool: pool} }

func (in PlanCardIssue) validate() error {
	switch {
	case in.ChannelID <= 0, in.PlanID <= 0:
		return fmt.Errorf("%w: a plan card needs a channel and a plan", ErrMalformed)
	case strings.TrimSpace(in.Database) == "", !planHashPattern.MatchString(in.PlanHash):
		return fmt.Errorf("%w: a plan card needs a database and the plan hash", ErrMalformed)
	case !in.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: the plan card has already expired", ErrMalformed)
	case !slices.Contains(in.Offered, PlanReject):
		return fmt.Errorf("%w: a plan card always offers reject", ErrMalformed)
	}
	for _, c := range in.Offered {
		if !c.Valid() {
			return fmt.Errorf("%w: unknown plan decision %q", ErrMalformed, c)
		}
	}
	upTo := slices.Contains(in.Offered, PlanApproveUpTo)
	if upTo != (in.UpTo != 0) || in.UpTo < 0 || in.UpTo > maxPlanOrdinal {
		return fmt.Errorf("%w: an up-to button needs its last step (1..%d) and only it "+
			"has one", ErrMalformed, maxPlanOrdinal)
	}
	return nil
}

func choiceStrings(cs []PlanChoice) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, string(c))
	}
	return out
}

// Issue records a plan card for one channel and returns its new token.
func (s *PlanCardStore) Issue(ctx context.Context, in PlanCardIssue) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("chatops: plan card token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	var upTo *int
	if in.UpTo > 0 {
		upTo = &in.UpTo
	}
	_, err := s.pool.Exec(ctx, `/* pg_sage */ INSERT INTO sage.plan_card_deliveries
		(token_sha256, channel_id, database_name, plan_id, plan_hash, title, offered_modes,
		 up_to_ordinal, expires_at)
		VALUES ($1, $2, $3, $4, $5, left($6, 512), $7, $8, $9)`, tokenHash(token),
		in.ChannelID, in.Database, in.PlanID, in.PlanHash, in.Title,
		choiceStrings(in.Offered), upTo, in.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("chatops: record plan card for plan %d: %w", in.PlanID, err)
	}
	return token, nil
}

const planCardColumns = `id, channel_id, database_name, plan_id, plan_hash, title,
	offered_modes, COALESCE(up_to_ordinal, 0), created_at, expires_at, used_at, used_by,
	COALESCE(decision, ''), message_id`

func scanPlanCard(row pgx.Row) (PlanCardDelivery, error) {
	var d PlanCardDelivery
	var offered []string
	var decision string
	err := row.Scan(&d.ID, &d.ChannelID, &d.Database, &d.PlanID, &d.PlanHash, &d.Title,
		&offered, &d.UpTo, &d.CreatedAt, &d.ExpiresAt, &d.UsedAt, &d.UsedBy, &decision,
		&d.MessageID)
	for _, c := range offered {
		d.Offered = append(d.Offered, PlanChoice(c))
	}
	d.Decision = PlanChoice(decision)
	return d, err
}

// Lookup returns the plan card a token names, used or not.
func (s *PlanCardStore) Lookup(ctx context.Context, token string) (PlanCardDelivery, error) {
	if !ValidCardToken(token) {
		return PlanCardDelivery{}, ErrCardUnknown
	}
	d, err := scanPlanCard(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+planCardColumns+`
		FROM sage.plan_card_deliveries WHERE token_sha256 = $1`, tokenHash(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanCardDelivery{}, ErrCardUnknown
	}
	if err != nil {
		return PlanCardDelivery{}, fmt.Errorf("chatops: read plan card: %w", err)
	}
	return d, nil
}

// Consume uses a plan card once for decision c, recording who decided from
// which chat message: a used card is ErrCardUsed, an expired one
// ErrCardExpired, a decision it did not offer ErrChoiceNotOffered.
func (s *PlanCardStore) Consume(ctx context.Context, token string, userID int, c PlanChoice,
	messageID int64) (PlanCardDelivery, error) {
	if !ValidCardToken(token) {
		return PlanCardDelivery{}, ErrCardUnknown
	}
	got, err := scanPlanCard(s.pool.QueryRow(ctx, `/* pg_sage */ UPDATE
		sage.plan_card_deliveries SET used_at = now(), used_by = $2, decision = $3,
		message_id = $4 WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()
		  AND $3 = ANY (offered_modes)
		RETURNING `+planCardColumns, tokenHash(token), userID, string(c), messageID))
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PlanCardDelivery{}, fmt.Errorf("chatops: use plan card: %w", err)
	}
	cur, lookupErr := s.Lookup(ctx, token)
	switch {
	case lookupErr != nil:
		return PlanCardDelivery{}, lookupErr
	case cur.UsedAt != nil:
		return PlanCardDelivery{}, ErrCardUsed
	case cur.Expired(time.Now()):
		return PlanCardDelivery{}, ErrCardExpired
	default:
		return PlanCardDelivery{}, ErrChoiceNotOffered
	}
}

// Release makes a card userID consumed usable again, when the plan store
// could not record that decision; any other card is ErrCardUsed (or
// ErrCardUnknown), so a release never frees another person's decision.
func (s *PlanCardStore) Release(ctx context.Context, token string, userID int) error {
	if !ValidCardToken(token) {
		return ErrCardUnknown
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_card_deliveries
		SET used_at = NULL, used_by = NULL, decision = NULL, message_id = 0
		WHERE token_sha256 = $1 AND used_at IS NOT NULL AND used_by = $2`,
		tokenHash(token), userID)
	if err != nil {
		return fmt.Errorf("chatops: release plan card: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := s.Lookup(ctx, token); err != nil {
		return err
	}
	return ErrCardUsed
}

// Revoke forgets a plan card whose message could not be delivered.
func (s *PlanCardStore) Revoke(ctx context.Context, token string) error {
	if !ValidCardToken(token) {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `/* pg_sage */ DELETE FROM sage.plan_card_deliveries
		WHERE token_sha256 = $1 AND used_at IS NULL`, tokenHash(token)); err != nil {
		return fmt.Errorf("chatops: revoke plan card: %w", err)
	}
	return nil
}
