package ask

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// persistTimeout bounds storing an answer after the caller's own
// deadline cut the run short.
const persistTimeout = 3 * time.Second

// Ask answers one question. The answer is stored in the caller's
// conversation (created when ConversationID is empty). A run the caller
// cancelled, or whose store failed, is stored only when it already took
// an action: that partial answer (status incomplete, stop cancelled) is
// returned together with the error. Invalid questions, a conversation the caller does not own
// and a disabled Ask Sage are errors; everything the model or provider
// does is a typed status of the answer. A read-only request is answered
// without the write tools whoever asks.
func (s *Service) Ask(ctx context.Context, c Caller, r Request) (Answer, error) {
	if !s.d.Config.Enabled {
		return Answer{}, ErrDisabled
	}
	c = c.For(r)
	question, err := checkRequest(c, r)
	if err != nil {
		return Answer{}, err
	}
	var history []Answer
	if r.ConversationID != "" {
		conv, err := s.store.conversation(ctx, c.Actor, r.ConversationID)
		if err != nil {
			return Answer{}, err
		}
		if conv.Messages >= MaxMessagesPerConversation {
			return Answer{}, fmt.Errorf("%w: the conversation is full (%d questions); "+
				"start a new one", ErrInvalid, conv.Messages)
		}
		if history, err = s.store.history(ctx, conv.ID, historyTurns); err != nil {
			return Answer{}, err
		}
	}
	a, runErr := s.answer(ctx, c, question, history)
	if runErr != nil && len(a.Actions) == 0 {
		return Answer{}, runErr
	}
	saved, err := s.persistAnswer(ctx, c, r.ConversationID, question, r.ReadOnly, a)
	if runErr == nil {
		return saved, err
	}
	if err != nil {
		s.d.Log("ERROR", "ask: the partial answer of a question %s cut off (%v) took %d "+
			"actions and could not be stored: %v", c.Actor, runErr, len(a.Actions), err)
	}
	return saved, runErr
}

func checkRequest(c Caller, r Request) (string, error) {
	if err := checkActor(c.Actor); err != nil {
		return "", err
	}
	q := strings.TrimSpace(r.Question)
	switch {
	case q == "":
		return "", fmt.Errorf("%w: the question is empty", ErrInvalid)
	case utf8.RuneCountInString(q) > MaxQuestionRunes:
		return "", fmt.Errorf("%w: the question is longer than %d characters", ErrInvalid,
			MaxQuestionRunes)
	case !utf8.ValidString(q):
		return "", fmt.Errorf("%w: the question is not valid UTF-8", ErrInvalid)
	}
	return q, nil
}

// Conversations lists the caller's conversations, newest first.
func (s *Service) Conversations(ctx context.Context, c Caller, limit int) ([]Conversation,
	error) {
	if err := checkActor(c.Actor); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	return s.store.conversations(ctx, c.Actor, limit)
}

// Thread is one of the caller's conversations with its answers.
func (s *Service) Thread(ctx context.Context, c Caller, id string) (Thread, error) {
	if err := checkActor(c.Actor); err != nil {
		return Thread{}, err
	}
	conv, err := s.store.conversation(ctx, c.Actor, id)
	if err != nil {
		return Thread{}, err
	}
	answers, err := s.store.answers(ctx, conv.ID)
	if err != nil {
		return Thread{}, err
	}
	return Thread{Conversation: conv, Answers: answers}, nil
}

// BudgetStatus is today's Ask Sage usage of the database and the caller.
func (s *Service) BudgetStatus(ctx context.Context, c Caller) (BudgetStatus, error) {
	return s.budget.Status(ctx, c.Actor)
}
