package clone

import (
	"context"
	"time"
)

// lab-05a: a provider that does not describe itself is treated as a full
// copy and needs clone.allow_full_copy. The receipt tests' adapter double
// stands in for DLE, so it declares DLE's kind; it has no Finder, so the
// reaper keeps "report, never guess" for it.
func (f *fakeInner) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindThinClone, TypicalReady: time.Second}, nil
}
