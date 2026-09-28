package sre

import (
	"context"
	"fmt"
)

// Service is the read (and pin) side of one database's investigations,
// for the API, MCP tools and exports. Every call is scoped to the
// coordinator's bound database identity.
type Service struct {
	coord *Coordinator
	store *PostgresStore
}

// NewService wraps a coordinator and its store.
func NewService(coord *Coordinator, store *PostgresStore) *Service {
	return &Service{coord: coord, store: store}
}

// Coordinator is the investigation loop behind the service.
func (s *Service) Coordinator() *Coordinator { return s.coord }

// scope is the bound database identity, binding now if needed.
func (s *Service) scope(ctx context.Context) (Scope, error) {
	if s == nil || s.coord == nil || s.store == nil {
		return Scope{}, fmt.Errorf("%w: investigations are not configured",
			ErrMetadataUnavailable)
	}
	return s.coord.Bind(ctx)
}

// List pages the database's investigations, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) (Page, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Page{}, err
	}
	return s.store.List(ctx, scope, f)
}
