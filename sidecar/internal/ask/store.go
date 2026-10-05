package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// store keeps conversations in the monitored database's sage schema
// (sage.ask_conversations, sage.ask_messages). A conversation belongs to
// one actor; another actor's conversation is "not found", never
// "forbidden", so its existence does not leak.
type store struct{ pool *pgxpool.Pool }

func newStore(pool *pgxpool.Pool) *store { return &store{pool: pool} }

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// conversation reads actor's conversation id.
func (st *store) conversation(ctx context.Context, actor, id string) (Conversation, error) {
	if !uuidPattern.MatchString(id) {
		return Conversation{}, fmt.Errorf("%w: conversation %q", ErrNotFound, id)
	}
	var c Conversation
	err := st.pool.QueryRow(ctx, `/* pg_sage */ SELECT c.id::text, c.title, c.created_at,
		c.updated_at, (SELECT count(*) FROM sage.ask_messages m
		               WHERE m.conversation_id = c.id)
		FROM sage.ask_conversations c WHERE c.id = $1::uuid AND c.actor = $2`, id,
		actor).Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.Messages)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Conversation{}, fmt.Errorf("%w: conversation %s", ErrNotFound, id)
	case err != nil:
		return Conversation{}, fmt.Errorf("%w: read conversation: %v", ErrUnavailable, err)
	}
	return c, nil
}

// history is the conversation's last n answers, oldest first.
func (st *store) history(ctx context.Context, id string, n int) ([]Answer, error) {
	rows, err := st.pool.Query(ctx, `/* pg_sage */ SELECT answer FROM (
		SELECT id, answer FROM sage.ask_messages WHERE conversation_id = $1::uuid
		ORDER BY id DESC LIMIT $2) last ORDER BY id`, id, n)
	if err != nil {
		return nil, fmt.Errorf("%w: read history: %v", ErrUnavailable, err)
	}
	return scanAnswers(rows)
}

func scanAnswers(rows pgx.Rows) ([]Answer, error) {
	defer rows.Close()
	out := []Answer{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("%w: scan answer: %v", ErrUnavailable, err)
		}
		var a Answer
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("%w: decode stored answer: %v", ErrUnavailable, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read answers: %v", ErrUnavailable, err)
	}
	return out, nil
}

// save stores a, creating the conversation when a.ConversationID is
// empty, and returns it with its ids.
func (st *store) save(ctx context.Context, actor string, a Answer) (Answer, error) {
	err := pgx.BeginFunc(ctx, st.pool, func(tx pgx.Tx) error {
		if a.ConversationID == "" {
			if err := tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.ask_conversations
				(actor, title) VALUES ($1, $2) RETURNING id::text`, actor,
				titleOf(a.Question)).Scan(&a.ConversationID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_conversations
			SET updated_at = now() WHERE id = $1::uuid`, a.ConversationID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT nextval(
			pg_get_serial_sequence('sage.ask_messages', 'id'))`).Scan(&a.ID); err != nil {
			return err
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.ask_messages (id,
			conversation_id, question, answer, status, tokens, created_at)
			VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)`, a.ID, a.ConversationID, a.Question,
			raw, a.Status, a.Tokens, a.CreatedAt)
		return err
	})
	if err != nil {
		return a, fmt.Errorf("%w: store the answer: %v", ErrUnavailable, err)
	}
	return a, nil
}

// conversations lists actor's conversations, newest first.
func (st *store) conversations(ctx context.Context, actor string, limit int) (
	[]Conversation, error) {
	rows, err := st.pool.Query(ctx, `/* pg_sage */ SELECT c.id::text, c.title, c.created_at,
		c.updated_at, (SELECT count(*) FROM sage.ask_messages m
		               WHERE m.conversation_id = c.id)
		FROM sage.ask_conversations c WHERE c.actor = $1
		ORDER BY c.updated_at DESC, c.id LIMIT $2`, actor, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list conversations: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt,
			&c.Messages); err != nil {
			return nil, fmt.Errorf("%w: scan conversation: %v", ErrUnavailable, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: list conversations: %v", ErrUnavailable, err)
	}
	return out, nil
}

// answers is the whole conversation, oldest first.
func (st *store) answers(ctx context.Context, id string) ([]Answer, error) {
	rows, err := st.pool.Query(ctx, `/* pg_sage */ SELECT answer FROM sage.ask_messages
		WHERE conversation_id = $1::uuid ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("%w: read conversation: %v", ErrUnavailable, err)
	}
	return scanAnswers(rows)
}

// titleOf is a conversation title from its first question.
func titleOf(q string) string { return clip(strings.Join(strings.Fields(q), " "), maxTitleRunes) }
