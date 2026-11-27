package fix

import "context"

type pool interface {
	Exec(ctx context.Context, sql string, args ...any) (any, error)
}

// Purge deletes from a user table: an unclassified write sink.
func Purge(ctx context.Context, p pool) error {
	_, err := p.Exec(ctx, "DELETE FROM public.t")
	return err
}
