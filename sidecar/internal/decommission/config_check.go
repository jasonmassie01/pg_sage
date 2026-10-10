package decommission

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/config"
	"gopkg.in/yaml.v3"
)

// LegacySection is the removed provisioner's top-level config key.
const LegacySection = "agentdb"

// ErrLiveProvisioningEnabled refuses a config that still turns live
// provisioning on: the code that honoured it is gone, and starting anyway
// would leave the operator believing billed resources are still managed.
var ErrLiveProvisioningEnabled = errors.New(LegacySection +
	".live_provisioning_enabled is true, but AgentDB provisioning was removed and " +
	"pg_sage no longer creates, monitors or destroys those databases. Refusing to " +
	"start: follow " + SpecRef + " (drain on the previous version, review the " +
	"inventory, then delete the " + LegacySection + ": section)")

func init() {
	config.RegisterRetiredSection(config.RetiredSection{Key: LegacySection,
		Check: checkLegacySection})
	config.RegisterRetiredSection(config.RetiredSection{Key: AckSection,
		Check: checkAckSection})
}

// legacyWarned makes the ignored-section warning appear once per process,
// not on every config reload.
var legacyWarned atomic.Bool

func resetLegacyWarning() { legacyWarned.Store(false) }

// checkLegacySection ignores the removed section with one warning, and
// refuses the config when it turns live provisioning on (G0-07).
func checkLegacySection(node *yaml.Node) ([]string, error) {
	if node != nil && node.Kind == yaml.MappingNode {
		var section struct {
			Live bool `yaml:"live_provisioning_enabled"`
		}
		if err := node.Decode(&section); err != nil {
			return nil, fmt.Errorf("read %s.live_provisioning_enabled: %w", LegacySection, err)
		}
		if section.Live {
			return nil, ErrLiveProvisioningEnabled
		}
	}
	if !legacyWarned.CompareAndSwap(false, true) {
		return nil, nil
	}
	return []string{fmt.Sprintf("WARNING: the %s: config section is ignored: AgentDB "+
		"provisioning was removed (%s). Review the inventory at GET %s, then delete "+
		"the section", LegacySection, SpecRef, InventoryPath)}, nil
}

// checkAckSection refuses a malformed acknowledgement; startup records a
// well-formed one (Startup).
func checkAckSection(node *yaml.Node) ([]string, error) {
	_, err := parseAckNode(node)
	return nil, err
}
