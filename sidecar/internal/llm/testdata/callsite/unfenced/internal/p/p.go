package p

import "context"

type client interface {
	Chat(ctx context.Context, system, user string, max int) (string, int, error)
}

// Ask sends database text with no rule.
func Ask(ctx context.Context, c client, query string) {
	_, _, _ = c.Chat(ctx, "You are a DBA.", query, 10)
}
