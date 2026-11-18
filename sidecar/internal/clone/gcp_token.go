package clone

import (
	"context"
	"errors"
	"fmt"
)

// gcpToken is a token source from chain whose first token is usable, or
// ErrCloneUnavailable naming adapter: without Google credentials no clone
// can be made or deleted, so rehearsals are recommend-only. It is the GCP
// token seam cloudsql_clone (lab-05e) and alloydb_restore (lab-05f) share.
func gcpToken(ctx context.Context, chain func(context.Context) (CloudSQLTokenSource, error),
	adapter string) (CloudSQLTokenSource, error) {
	const hint = "; rehearsals are recommend-only until then (set " +
		"GOOGLE_APPLICATION_CREDENTIALS, run gcloud auth application-default login, or " +
		"run with a service account)"
	if chain == nil {
		return nil, fmt.Errorf("%w: %s has no Google credentials%s",
			ErrCloneUnavailable, adapter, hint)
	}
	src, err := chain(ctx)
	if err == nil && src == nil {
		err = errors.New("the credential chain returned no token source")
	}
	if err == nil {
		var tok string
		tok, err = src.Token(ctx)
		if err == nil && blank(tok) {
			err = errors.New("the credential chain returned an empty access token")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no usable Google credentials: %w%s",
			ErrCloneUnavailable, adapter, err, hint)
	}
	return src, nil
}
