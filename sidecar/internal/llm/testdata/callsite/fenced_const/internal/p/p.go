package p

import (
	"context"

	"github.com/pg-sage/sidecar/internal/llm"
)

type client interface {
	Chat(ctx context.Context, system, user string, max int) (string, int, error)
}

const system = "You are a DBA. " + llm.UntrustedDataRule

func Ask(ctx context.Context, c client, query string) {
	_, _, _ = c.Chat(ctx, system, query, 10)
}
