package tuning

import "math"

// The write-cost model: what one more index costs on writes, from the
// table's write rates and the new index's entry width. Index entries are
// written for inserts and non-HOT updates; deletes write none (vacuum
// removes the entries later).

// WriteCostInput is a table's write activity and the candidate's width.
type WriteCostInput struct {
	InsertsPerSec    float64
	UpdatesPerSec    float64
	HotUpdatesPerSec float64
	DeletesPerSec    float64
	Indexes          int
	LiveTuples       int64
	EntryBytes       int
}

// WriteCost is the estimate.
type WriteCost struct {
	IndexWritesPerSec       float64 // entries the existing indexes write
	NewIndexWritesPerSec    float64 // entries the new index would write
	NewIndexBytesPerSec     float64
	ShareOfIndexMaintenance float64 // the new index's share of all entries
	HOTRatio                float64 // HOT updates / updates
	EstimatedNewIndexBytes  int64   // leaf size at the default 90% fill
}

// btreeFill is the default B-tree leaf fill factor.
const btreeFill = 0.9

// EstimateWriteCost estimates the write cost of one more index; negative
// inputs count as zero and HOT updates are capped at the updates.
func EstimateWriteCost(in WriteCostInput) WriteCost {
	ins, upd := math.Max(in.InsertsPerSec, 0), math.Max(in.UpdatesPerSec, 0)
	hot := math.Min(math.Max(in.HotUpdatesPerSec, 0), upd)
	perIndex := ins + upd - hot
	var out WriteCost
	out.IndexWritesPerSec = perIndex * float64(max(in.Indexes, 0))
	out.NewIndexWritesPerSec = perIndex
	out.NewIndexBytesPerSec = perIndex * float64(max(in.EntryBytes, 0))
	if all := out.IndexWritesPerSec + out.NewIndexWritesPerSec; all > 0 {
		out.ShareOfIndexMaintenance = out.NewIndexWritesPerSec / all
	}
	if upd > 0 {
		out.HOTRatio = hot / upd
	}
	rows, width := max(in.LiveTuples, 0), max(in.EntryBytes, 0)
	out.EstimatedNewIndexBytes = int64(math.Ceil(float64(rows) * float64(width) / btreeFill))
	return out
}

// entryBytes is a B-tree entry's size for key columns of the given average
// widths: the 8-byte tuple header and 8-byte item pointer plus the data,
// aligned to 8 bytes. An unknown (non-positive) width counts as zero.
func entryBytes(widths []int) int {
	n := 16
	for _, w := range widths {
		n += max(w, 0)
	}
	return (n + 7) / 8 * 8
}
