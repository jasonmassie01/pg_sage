package p

import (
	"context"

	"github.com/pg-sage/sidecar/internal/llm"
)

type model interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message) (llm.ToolResult, error)
}

// Forward sends messages a caller built, with no annotation.
func Forward(ctx context.Context, m model, msgs []llm.Message) {
	_, _ = m.ChatWithTools(ctx, msgs)
}
