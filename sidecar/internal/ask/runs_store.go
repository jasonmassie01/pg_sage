package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// sage.ask_runs: every write is guarded by the row's state, so a run ends
// in exactly one terminal state whoever (the run, a cancel, a restart)
// gets there first.

// runInsert is a run to queue. A conversation is locked while its active
// runs are counted, so two starts on it cannot both pass.
type runInsert struct {
	id, actor, conversation, question, instance string
	readOnly                                    bool
	staleBefore                                 time.Time
}

// insertRun stores a queued run and returns its conversation, created
// when in.conversation is empty.
func (st *store) insertRun(ctx context.Context, in runInsert) (string, error) {
	conv := in.conversation
	err := pgx.BeginFunc(ctx, st.pool, func(tx pgx.Tx) error {
		var err error
		if conv == "" {
			err = tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.ask_conversations
				(actor, title) VALUES ($1, $2) RETURNING id::text`, in.actor,
				titleOf(in.question)).Scan(&conv)
		} else {
			err = lockConversationForRun(ctx, tx, in)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.ask_runs (id,
			conversation_id, actor, question, read_only, state, instance)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'queued', $6)`, in.id, conv, in.actor,
			in.question, in.readOnly, in.instance)
		return err
	})
	switch {
	case err == nil:
		return conv, nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid), errors.Is(err, ErrConflict):
		return "", err
	}
	return "", fmt.Errorf("%w: queue the run: %v", ErrUnavailable, err)
}

// lockConversationForRun locks the actor's conversation and refuses a
// full one or one with an active run (a row updated since staleBefore).
func lockConversationForRun(ctx context.Context, tx pgx.Tx, in runInsert) error {
	var one int
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT 1 FROM sage.ask_conversations
		WHERE id = $1::uuid AND actor = $2 FOR UPDATE`, in.conversation, in.actor).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: conversation %s", ErrNotFound, in.conversation)
	}
	if err != nil {
		return err
	}
	var messages int
	var active bool
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT
		(SELECT count(*) FROM sage.ask_messages WHERE conversation_id = $1::uuid),
		EXISTS (SELECT 1 FROM sage.ask_runs WHERE conversation_id = $1::uuid
		        AND state IN ('queued', 'running') AND updated_at > $2)`, in.conversation,
		in.staleBefore).Scan(&messages, &active); err != nil {
		return err
	}
	switch {
	case messages >= MaxMessagesPerConversation:
		return fmt.Errorf("%w: the conversation is full (%d questions); start a new one",
			ErrInvalid, messages)
	case active:
		return fmt.Errorf("%w: a question is still being answered in this conversation; "+
			"wait for it or cancel it", ErrConflict)
	}
	return nil
}

// run reads actor's run id with its stored answer.
func (st *store) run(ctx context.Context, actor, id string) (Run, error) {
	var r Run
	var progress, answer []byte
	err := st.pool.QueryRow(ctx, `/* pg_sage */ SELECT r.id::text,
		r.conversation_id::text, r.state, r.question, r.read_only, r.progress, r.message_id,
		COALESCE(r.error, ''), r.created_at, r.updated_at, r.finished_at, m.answer
		FROM sage.ask_runs r LEFT JOIN sage.ask_messages m ON m.id = r.message_id
		WHERE r.id = $1::uuid AND r.actor = $2`, id, actor).Scan(&r.ID, &r.ConversationID,
		&r.State, &r.Question, &r.ReadOnly, &progress, &r.MessageID, &r.Error, &r.CreatedAt,
		&r.UpdatedAt, &r.FinishedAt, &answer)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Run{}, fmt.Errorf("%w: run %s", ErrNotFound, id)
	case err != nil:
		return Run{}, fmt.Errorf("%w: read run: %v", ErrUnavailable, err)
	}
	r.Progress = []RunStep{}
	if err := json.Unmarshal(progress, &r.Progress); err != nil || r.Progress == nil {
		return Run{}, fmt.Errorf("%w: decode the run's progress: %v", ErrUnavailable, err)
	}
	if answer != nil {
		var a Answer
		if err := json.Unmarshal(answer, &a); err != nil {
			return Run{}, fmt.Errorf("%w: decode the run's answer: %v", ErrUnavailable, err)
		}
		r.Answer = &a
	}
	return r, nil
}

// markRunning moves a queued run to running; false when it is no longer
// queued (cancelled before it started).
func (st *store) markRunning(ctx context.Context, id string) (bool, error) {
	tag, err := st.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_runs
		SET state = 'running', updated_at = now()
		WHERE id = $1::uuid AND state = 'queued'`, id)
	if err != nil {
		return false, fmt.Errorf("%w: start the run: %v", ErrUnavailable, err)
	}
	return tag.RowsAffected() == 1, nil
}

// appendProgress stores one tool call (beyond maxRunSteps only the update
// time moves); false when the run is no longer running.
func (st *store) appendProgress(ctx context.Context, id string, step RunStep) (bool,
	error) {
	raw, err := json.Marshal(step)
	if err != nil {
		return false, err
	}
	tag, err := st.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_runs
		SET progress = CASE WHEN jsonb_array_length(progress) < $3
		                    THEN progress || jsonb_build_array($2::jsonb)
		                    ELSE progress END,
		    updated_at = now()
		WHERE id = $1::uuid AND state = 'running'`, id, raw, maxRunSteps)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// finishRun stores a run's terminal state, its answer and its error. A
// run already cancelled (by another instance) keeps that state and gains
// the answer it stored.
func (st *store) finishRun(ctx context.Context, id, state string, msg *int64,
	errText string) error {
	ctx, cancel := context.WithTimeout(ctx, persistTimeout)
	defer cancel()
	_, err := st.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_runs SET
		state = CASE WHEN state IN ('queued', 'running') THEN $2 ELSE state END,
		message_id = COALESCE(message_id, $3),
		error = CASE WHEN state IN ('queued', 'running') THEN NULLIF($4, '') ELSE error END,
		finished_at = COALESCE(finished_at, now()), updated_at = now()
		WHERE id = $1::uuid AND (state IN ('queued', 'running') OR state = 'cancelled')`,
		id, state, msg, errText)
	if err != nil {
		return fmt.Errorf("%w: store the run's end (%s): %v", ErrUnavailable, state, err)
	}
	return nil
}

// cancelRun marks a run that runs elsewhere (or nowhere) cancelled.
func (st *store) cancelRun(ctx context.Context, id string) error {
	_, err := st.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_runs
		SET state = 'cancelled', finished_at = now(), updated_at = now()
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id)
	if err != nil {
		return fmt.Errorf("%w: cancel the run: %v", ErrUnavailable, err)
	}
	return nil
}

// interruptStale marks instance's queued or running runs not updated
// since before, other than live, interrupted; their progress stays as
// the record.
func (st *store) interruptStale(ctx context.Context, instance string, before time.Time,
	live []string) (int64, error) {
	tag, err := st.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_runs
		SET state = 'interrupted', error = 'the sidecar restarted before the run ended',
		    finished_at = now(), updated_at = now()
		WHERE instance = $1 AND state IN ('queued', 'running') AND updated_at < $2
		      AND NOT (id = ANY ($3::uuid[]))`,
		instance, before, live)
	if err != nil {
		return 0, fmt.Errorf("%w: recover abandoned runs: %v", ErrUnavailable, err)
	}
	return tag.RowsAffected(), nil
}
