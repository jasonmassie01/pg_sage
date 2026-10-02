package store

import (
	"fmt"
	"regexp"
)

// ChatOps channel settings. A Telegram channel posts through a bot into
// one chat; its optional webhook_secret is the secret token Telegram
// sends with callback deliveries. A Slack channel with interactive=true
// offers Approve/Deny buttons, so it must carry the signing secret its
// callbacks are verified with.
var (
	telegramTokenPattern  = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{1,128}$`)
	telegramChatPattern   = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,64})$`)
	telegramSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
)

func validateTelegramConfig(config map[string]string) error {
	if !telegramTokenPattern.MatchString(config["bot_token"]) {
		return fmt.Errorf("%w: telegram channel requires bot_token "+
			"(<bot id>:<secret>)", ErrValidation)
	}
	if !telegramChatPattern.MatchString(config["chat_id"]) {
		return fmt.Errorf("%w: telegram channel requires a numeric chat_id "+
			"or an @channel name", ErrValidation)
	}
	if s, ok := config["webhook_secret"]; ok && s != "" &&
		!telegramSecretPattern.MatchString(s) {
		return fmt.Errorf("%w: webhook_secret may use only letters, digits, "+
			"_ and - (1-256)", ErrValidation)
	}
	return nil
}

func validateSlackInteractive(config map[string]string) error {
	switch config["interactive"] {
	case "", "false":
		return nil
	case "true":
		if config["signing_secret"] == "" {
			return fmt.Errorf("%w: an interactive slack channel requires "+
				"signing_secret", ErrValidation)
		}
		return nil
	default:
		return fmt.Errorf("%w: interactive must be true or false", ErrValidation)
	}
}
