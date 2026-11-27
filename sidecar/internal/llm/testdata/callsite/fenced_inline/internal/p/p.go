package p

import (
	"context"

	"github.com/pg-sage/sidecar/internal/llm"
)

type client interface {
	Chat(ctx context.Context, system, user string, max int) (string, int, error)
}

func Ask(ctx context.Context, c client, query string) {
	_, _, _ = c.Chat(ctx, "You are a DBA. "+llm.UntrustedDataRule, query, 10)
}
