package probes

import (
	"errors"
	"fmt"
)

// Errors of CheckArgs.
var (
	ErrUnknownProbe = errors.New("unknown probe")
	ErrInvalidArgs  = errors.New("invalid probe arguments")
)

// CheckArgs reports whether id is a catalog probe of this registry and
// args fit its argument kind and bounds. A model-proposed probe is
// checked here before anything runs. A nil registry knows no probes.
func (r *Registry) CheckArgs(id ID, args Args) error {
	spec, ok := r.Spec(id)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProbe, id)
	}
	if err := args.validate(spec.Args); err != nil {
		return fmt.Errorf("%w for %s: %v", ErrInvalidArgs, id, err)
	}
	return nil
}
