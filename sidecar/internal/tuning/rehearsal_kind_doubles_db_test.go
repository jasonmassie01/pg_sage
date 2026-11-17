package tuning

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05a: a provider that does not describe itself is a full copy and
// needs clone.allow_full_copy. The receipt tests' probe stands in for DLE
// (adapter "dle"), so it declares DLE's kind.
func (p *statusProbe) Describe(context.Context) (clone.Capability, error) {
	return clone.Capability{Kind: clone.KindThinClone, TypicalReady: time.Second}, nil
}
