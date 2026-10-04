package replay

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// The tuning / held-out split (roadmap 2.4). A case's split is a stable
// hash of its id: the first 8 bytes of sha256(splitSalt + id), as a
// big-endian integer, modulo 100 is its bucket, and buckets below
// HeldOutPercent are held out. Gates and the model-root override rule
// read held-out cases only; thresholds may be tuned on the tuning set.
// split.lock pins every case's split, so renaming a case (which re-draws
// its split) or adding one shows in review.

// Splits.
const (
	SplitHeldOut = modellift.SplitHeldOut
	SplitTuning  = modellift.SplitTuning
	// SplitAll selects every case.
	SplitAll = "all"
	// HeldOutPercent is the share of buckets held out.
	HeldOutPercent = 50

	splitSalt = "pg_sage.replay.split.v1:"
)

// SplitBucket is the case id's bucket, 0 to 99.
func SplitBucket(id string) int {
	sum := sha256.Sum256([]byte(splitSalt + id))
	return int(binary.BigEndian.Uint64(sum[:8]) % 100)
}

// SplitOf is the case id's split: SplitHeldOut or SplitTuning.
func SplitOf(id string) string { return splitForBucket(SplitBucket(id)) }

func splitForBucket(bucket int) string {
	if bucket < HeldOutPercent {
		return SplitHeldOut
	}
	return SplitTuning
}

// FilterSplit keeps the cases of split ("" or SplitAll keeps every case).
func FilterSplit(cases []Case, split string) ([]Case, error) {
	switch split {
	case "", SplitAll:
		return cases, nil
	case SplitHeldOut, SplitTuning:
	default:
		return nil, fmt.Errorf("unknown split %q (want %s, %s or %s)", split, SplitAll,
			SplitHeldOut, SplitTuning)
	}
	out := make([]Case, 0, len(cases))
	for _, c := range cases {
		if SplitOf(c.ID) == split {
			out = append(out, c)
		}
	}
	return out, nil
}
