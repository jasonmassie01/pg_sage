package p

import (
	"context"

	"github.com/pg-sage/sidecar/internal/llm"
)

type client interface {
	Chat(ctx context.Context, system, user string, max int) (string, int, error)
}

const rules = "You are a DBA. "

func build() string { return rules + tail }

var tail = llm.UntrustedDataRule

func Ask(ctx context.Context, c client, query string) {
	sys := build()
	_, _, _ = c.Chat(ctx, sys, query, 10)
}
