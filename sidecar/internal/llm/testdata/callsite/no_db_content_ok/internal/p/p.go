package p

import "context"

type client interface {
	Chat(ctx context.Context, system, user string, max int) (string, int, error)
}

// Ping sends a fixed prompt.
//
//llm:no-db-content a fixed prompt and a nonce
func Ping(ctx context.Context, c client) {
	_, _, _ = c.Chat(ctx, "Reply OK.", "nonce", 2)
}
