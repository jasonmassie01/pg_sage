package llm

import (
	"context"

	"github.com/pg-sage/sidecar/internal/config"
)

func (*Client) Name() string { return "llm" }

func (c *Client) Prepare(
	_ context.Context,
	active config.ConfigSnapshot,
	desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	previous := config.LLMConfig{}
	if active.Config != nil {
		previous = active.Config.LLM
	}
	next := config.LLMConfig{}
	if desired.Config != nil {
		next = desired.Config.LLM
	}
	return &preparedClientConfig{
		client: c, previous: previous, next: next,
	}, nil
}

type preparedClientConfig struct {
	client   *Client
	previous config.LLMConfig
	next     config.LLMConfig
}

func (p *preparedClientConfig) Commit(context.Context) error {
	p.client.Reconfigure(&p.next)
	return nil
}

func (p *preparedClientConfig) Rollback(context.Context) error {
	p.client.Reconfigure(&p.previous)
	return nil
}

func (*preparedClientConfig) Drain(context.Context) error { return nil }
