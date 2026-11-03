package ask

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/sre"
)

func TestSpillGroundingRequiresExactInterval(t *testing.T) {
	evidence := []agentloop.Evidence{{ID: "finding:13", Text: `{"delta_temp_blks_written":2834,"delta_temp_blks_read":2794,"delta_calls":1,"interval_seconds":59.998903955}`}}
	for _, text := range []string{
		"The query wrote 2,834 temporary blocks and read 2,794 temporary blocks.",
		"The collection interval was 59.998903955 seconds.",
	} {
		if err := sre.GroundClaim(text, evidence); err != nil {
			t.Fatalf("exact evidence rejected: %v", err)
		}
	}
	err := sre.GroundClaim("The query wrote 2,834 temporary blocks and read 2,794 temporary blocks in about 60 seconds.", evidence)
	if !errors.Is(err, sre.ErrUngroundedNumber) || !strings.Contains(err.Error(), "60") {
		t.Fatalf("rounded interval error=%v, want ungrounded number 60", err)
	}
}
