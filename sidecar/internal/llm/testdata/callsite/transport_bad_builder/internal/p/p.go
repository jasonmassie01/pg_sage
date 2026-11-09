package p

import (
	"context"

	"github.com/pg-sage/sidecar/internal/llm"
)

type model interface {
	ChatWithTools(ctx context.Context, msgs []llm.Message) (llm.ToolResult, error)
}

func build() []llm.Message {
	return []llm.Message{{Role: "system", Content: "DBA. " + llm.UntrustedDataRule}}
}

func plain() []llm.Message { return []llm.Message{{Role: "system", Content: "DBA."}} }

// Forward sends messages a caller built.
//
//llm:transport internal/p.build internal/p.plain
func Forward(ctx context.Context, m model, msgs []llm.Message) {
	_, _ = m.ChatWithTools(ctx, msgs)
}
