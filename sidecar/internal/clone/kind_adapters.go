package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// Adapter names a receipt and clone.provider use (lab-05a). The four
// full-copy restores replace clone.provider snapshot; lab-05d to lab-05g
// build them on SnapshotProvider.
const (
	DLEAdapter            = "dle"
	RDSRestoreAdapter     = "rds_restore"
	CloudSQLCloneAdapter  = "cloudsql_clone"
	AlloyDBRestoreAdapter = "alloydb_restore"
	AzureRestoreAdapter   = "azure_restore"
)

// AdapterNames are every registered clone adapter name.
func AdapterNames() []string {
	return []string{DLEAdapter, LakebaseAdapter, NeonAdapter, RDSRestoreAdapter,
		CloudSQLCloneAdapter, AlloyDBRestoreAdapter, AzureRestoreAdapter}
}

// Typical create times, used for the cost label and operator guidance.
const (
	dleTypicalReady      = 15 * time.Second
	lakebaseTypicalReady = 2 * time.Minute
	// SnapshotTypicalReady is a full-copy restore's usual time.
	SnapshotTypicalReady = 45 * time.Minute
	// SnapshotMaxTTL bounds a full-copy clone's life.
	SnapshotMaxTTL = 24 * time.Hour
)

// Describe: a DLE clone is a thin clone on the DLE host.
func (p *DLEProvider) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindThinClone, TypicalReady: dleTypicalReady}, nil
}

// Describe: a Lakebase rehearsal clone is a branch that expires server
// side after its TTL.
func (p *LakebaseBranchProvider) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindBranch, TypicalReady: lakebaseTypicalReady,
		MaxTTL: p.cfg.TTL}, nil
}

// Describe: a snapshot restore is always a full copy. An API that
// describes itself sets the times; its kind is never trusted over that.
func (p *SnapshotProvider) Describe(ctx context.Context) (Capability, error) {
	c := Capability{TypicalReady: SnapshotTypicalReady, MaxTTL: SnapshotMaxTTL}
	if d, ok := p.api.(Describer); ok {
		got, err := d.Describe(ctx)
		if err != nil {
			return Capability{}, err
		}
		if got.TypicalReady > 0 {
			c.TypicalReady = got.TypicalReady
		}
		if got.MaxTTL > 0 {
			c.MaxTTL = got.MaxTTL
		}
	}
	c.Kind = KindFullCopy
	return c, nil
}

// Finder is a provider that can look up a resource by the name the
// receipt gave it (CloneSpec.ResourceName), so a create whose outcome is
// unknown can be resolved instead of guessed.
type Finder interface {
	FindByName(ctx context.Context, name string) (ref string, found bool, err error)
}

var receiptUUIDPrefix = regexp.MustCompile(`^[0-9a-f]{8}-`)

// ResourceName is the provider resource name of a receipt's clone,
// "sage-<purpose>-<8 hex>": the receipt name's random part, or a hash of
// a name receiptName did not make.
func ResourceName(r Receipt) string {
	purpose := strings.ReplaceAll(r.Purpose, "_", "-")
	rest := strings.TrimPrefix(r.Name, r.Purpose+"-")
	if rest != r.Name && receiptUUIDPrefix.MatchString(rest) {
		return "sage-" + purpose + "-" + rest[:8]
	}
	sum := sha256.Sum256([]byte(r.Name))
	return "sage-" + purpose + "-" + hex.EncodeToString(sum[:4])
}
