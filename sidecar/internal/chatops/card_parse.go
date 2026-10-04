package chatops

import (
	"fmt"
	"regexp"
	"strings"
)

// Approval-card buttons (roadmap 1.5). A card's buttons carry only the
// decision and an opaque card token: 128 random bits, base64url without
// padding (22 characters). The token names one card sent to one channel;
// the server keeps only its SHA-256.

// Slack button action ids of an approval card.
const (
	SlackCardApproveAction = "sage_card_approve"
	SlackCardRejectAction  = "sage_card_reject"
	SlackCardSnoozeAction  = "sage_card_snooze"
)

// cardDataPrefix starts every card's Telegram callback data:
// "sage:ca:<token>" (approve), "sage:cr:<token>" (reject) or
// "sage:cs:<token>" (snooze). At most 30 bytes, within Telegram's 64.
const cardDataPrefix = "sage:c"

var cardTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// ValidCardToken reports whether s has the shape of a card token.
func ValidCardToken(s string) bool { return cardTokenPattern.MatchString(s) }

// CardCallbackData is the Telegram button payload of a card decision.
func CardCallbackData(d Decision, token string) string {
	verb := "r"
	switch d {
	case DecisionApprove:
		verb = "a"
	case DecisionSnooze:
		verb = "s"
	}
	return cardDataPrefix + verb + ":" + token
}

// setCardData reads card callback data into the action.
func (a *Action) setCardData(data string) error {
	rest := strings.TrimPrefix(data, cardDataPrefix)
	if len(rest) < 2 || rest[1] != ':' {
		return fmt.Errorf("%w: unknown card callback data", ErrMalformed)
	}
	switch rest[0] {
	case 'a':
		a.Decision = DecisionApprove
	case 'r':
		a.Decision = DecisionDeny
	case 's':
		a.Decision = DecisionSnooze
	default:
		return fmt.Errorf("%w: unknown card decision", ErrMalformed)
	}
	if token := rest[2:]; ValidCardToken(token) {
		a.CardToken = token
		return nil
	}
	return fmt.Errorf("%w: bad card token", ErrMalformed)
}
