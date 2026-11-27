package autonomy

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// scanHorizonRow scans one FreezeHorizonsSQL row the way a sampler
// reading answers it (probes.ScanFreezeHorizon, then newFreezeRow), for
// the unit tests that feed the custodian adapter rows. It keeps the error
// prefix of the per-table scan it replaced.
func scanHorizonRow(row interface{ Scan(...any) error }) (freezeRow, error) {
	h, err := probes.ScanFreezeHorizon(row)
	if err != nil {
		return freezeRow{}, fmt.Errorf("scan freeze response: %w", err)
	}
	return newFreezeRow(h), nil
}
