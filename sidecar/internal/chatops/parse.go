package chatops

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Decision is what the chat user chose.
type Decision string

// Decisions a callback can carry.
const (
	DecisionApprove Decision = "approve"
	DecisionDeny    Decision = "deny"
	// DecisionSnooze defers an approval card (cards only).
	DecisionSnooze Decision = "snooze"
)

// Slack button action ids of an approval message.
const (
	SlackApproveAction = "sage_approve"
	SlackDenyAction    = "sage_deny"
)

var (
	// ErrMalformed means the callback is not a well-formed approval
	// decision.
	ErrMalformed = errors.New("chatops: malformed callback")
	// ErrNotCallback means a Telegram update is not a button press (a
	// chat message, an edit): it is acknowledged and ignored.
	ErrNotCallback = errors.New("chatops: not a callback")
)

// Action is one verified-format decision from a chat provider. Who
// decided comes from the provider's envelope, never from the payload
// the button carried.
type Action struct {
	Provider    string
	TeamID      string // Slack workspace; empty for Telegram
	ChatID      string // Telegram chat; empty for Slack
	UserID      string
	UserName    string
	Decision    Decision
	ProposalID  string // a Sage SRE proposal button
	CardToken   string // an approval-card button
	Nonce       string // replay key, unique per delivery
	ResponseURL string // Slack follow-up URL
	CallbackID  string // Telegram callback query id
	MessageID   int64  // Telegram message carrying the buttons
}

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type slackEnvelope struct {
	Type string `json:"type"`
	Team *struct {
		ID string `json:"id"`
	} `json:"team"`
	User *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

// ParseSlack reads a block_actions interaction (form field "payload")
// carrying exactly one approve or deny button press.
func ParseSlack(body []byte) (Action, error) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return Action{}, fmt.Errorf("%w: body is not a form", ErrMalformed)
	}
	raw := form.Get("payload")
	if raw == "" {
		return Action{}, fmt.Errorf("%w: no payload", ErrMalformed)
	}
	var p slackEnvelope
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Action{}, fmt.Errorf("%w: payload is not JSON", ErrMalformed)
	}
	if p.Type != "block_actions" || p.Team == nil || p.Team.ID == "" ||
		p.User == nil || p.User.ID == "" || len(p.Actions) != 1 {
		return Action{}, fmt.Errorf("%w: not one button press by a known user", ErrMalformed)
	}
	a := p.Actions[0]
	if a.ActionTS == "" {
		return Action{}, fmt.Errorf("%w: no action timestamp", ErrMalformed)
	}
	out := Action{Provider: ProviderSlack, TeamID: p.Team.ID, UserID: p.User.ID,
		UserName: p.User.Username, Nonce: p.Team.ID + ":" + p.User.ID + ":" + a.ActionTS,
		ResponseURL: p.ResponseURL}
	if err := out.setSlackDecision(a.ActionID, a.Value); err != nil {
		return Action{}, err
	}
	return out, nil
}

// slackCardActions are the approval-card buttons and their decisions.
var slackCardActions = map[string]Decision{
	SlackCardApproveAction: DecisionApprove,
	SlackCardRejectAction:  DecisionDeny,
	SlackCardSnoozeAction:  DecisionSnooze,
}

// setSlackDecision reads a button: a card button carries a card token, a
// proposal button a proposal id.
func (out *Action) setSlackDecision(actionID, value string) error {
	if d, ok := slackCardActions[actionID]; ok {
		if !ValidCardToken(value) {
			return fmt.Errorf("%w: bad card token", ErrMalformed)
		}
		out.Decision, out.CardToken = d, value
		return nil
	}
	switch actionID {
	case SlackApproveAction:
		out.Decision = DecisionApprove
	case SlackDenyAction:
		out.Decision = DecisionDeny
	default:
		return fmt.Errorf("%w: unknown action", ErrMalformed)
	}
	if !uuidPattern.MatchString(value) {
		return fmt.Errorf("%w: bad proposal id", ErrMalformed)
	}
	out.ProposalID = strings.ToLower(value)
	return nil
}

type telegramEnvelope struct {
	UpdateID *int64 `json:"update_id"`
	Callback *struct {
		ID   string `json:"id"`
		From *struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

// ParseTelegram reads a webhook update. Only callback queries carrying
// CallbackData are decisions; other updates are ErrNotCallback.
func ParseTelegram(body []byte) (Action, error) {
	var u telegramEnvelope
	if err := json.Unmarshal(body, &u); err != nil {
		return Action{}, fmt.Errorf("%w: update is not JSON", ErrMalformed)
	}
	if u.Callback == nil {
		return Action{}, ErrNotCallback
	}
	c := u.Callback
	if u.UpdateID == nil || c.From == nil || c.From.ID == 0 || c.ID == "" {
		return Action{}, fmt.Errorf("%w: no update id or sender", ErrMalformed)
	}
	a := Action{Provider: ProviderTelegram, UserID: strconv.FormatInt(c.From.ID, 10),
		UserName: c.From.Username, CallbackID: c.ID,
		Nonce: "update:" + strconv.FormatInt(*u.UpdateID, 10)}
	var err error
	if strings.HasPrefix(c.Data, cardDataPrefix) {
		err = a.setCardData(c.Data)
	} else {
		a.Decision, a.ProposalID, err = parseCallbackData(c.Data)
	}
	if err != nil {
		return Action{}, err
	}
	if c.Message != nil {
		a.ChatID = strconv.FormatInt(c.Message.Chat.ID, 10)
		a.MessageID = c.Message.MessageID
	}
	return a, nil
}

// CallbackData is the Telegram button payload of a decision (at most 64
// bytes): "sage:a:<proposal id>" or "sage:d:<proposal id>".
func CallbackData(d Decision, proposalID string) string {
	verb := "d"
	if d == DecisionApprove {
		verb = "a"
	}
	return "sage:" + verb + ":" + proposalID
}

func parseCallbackData(data string) (Decision, string, error) {
	rest, ok := strings.CutPrefix(data, "sage:")
	if !ok || len(rest) < 3 || rest[1] != ':' {
		return "", "", fmt.Errorf("%w: unknown callback data", ErrMalformed)
	}
	var d Decision
	switch rest[0] {
	case 'a':
		d = DecisionApprove
	case 'd':
		d = DecisionDeny
	default:
		return "", "", fmt.Errorf("%w: unknown decision", ErrMalformed)
	}
	id := rest[2:]
	if !uuidPattern.MatchString(id) {
		return "", "", fmt.Errorf("%w: bad proposal id", ErrMalformed)
	}
	return d, strings.ToLower(id), nil
}
