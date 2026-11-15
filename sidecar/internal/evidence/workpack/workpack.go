// Package workpack builds the workload evidence pack (evid-02a): one
// database's workload over a window of 14 to 100 days, as canonical JSON
// section files sealed with the evidence pack format (evidence.SealFiles)
// and stored once per database and content hash in sage.workload_packs.
//
// The pack's content_sha256 is the SHA-256 of its sorted "name sha256"
// lines: the same window and data give the same hash, whenever and by
// whomever it is built (build time and builder live only in the stored
// row). Every fact has an evidence id derived from what it is about
// (section/kind/key), never from a row id, so a plan can cite it and a
// reader can resolve it against the pack it pinned (Resolve).
//
// A build only reads: the rollup tables and snapshots of the database's
// history store (histstore), in one read-only transaction bounded by a
// statement timeout, and (evid-02b) the monitored database's catalog and
// pg_sage tables, each section in its own bounded read-only transaction
// (catalog.go).
package workpack

import (
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Format names the pack layout.
const Format = "pg_sage-workload/1"

// Window bounds and the default: every hour-of-week bucket at least twice,
// at most R2 §4's 100 days; the default is baseline.MinWeeks weeks plus one.
const (
	MinWindow     = 14 * 24 * time.Hour
	MaxWindow     = 100 * 24 * time.Hour
	DefaultWindow = (baseline.MinWeeks + 1) * 7 * 24 * time.Hour
)

// Pack bounds.
const (
	MaxFamilies          = 50
	MaxQueryIDs          = 500
	TopQueryIDsPerFamily = 20
	MaxTables            = 50
	// MaxBodyBytes bounds the stored gzip body (32 MB).
	MaxBodyBytes = 32 << 20
)

// Section file names.
const (
	fileHeader   = "header.json"
	fileCoverage = "coverage.json"
	fileWorkload = "workload.json"
	fileGrowth   = "growth.json"
)

// Errors a caller can tell apart.
var (
	ErrWindow          = errors.New("workpack: invalid window")
	ErrUnknownEvidence = errors.New("workpack: unknown evidence id")
	ErrPackTooLarge    = errors.New("workpack: pack too large")
	ErrCorrupt         = errors.New("workpack: stored pack does not match its manifest")
	ErrNotFound        = errors.New("workpack: no such pack")
)

// Capabilities are the collector's optional pg_stat_statements columns.
type Capabilities struct {
	WALColumns  bool `json:"wal_columns"`
	PlanColumns bool `json:"plan_columns"`
}

// Request is what a build is about. Location is the hour-of-week zone
// (the anomaly rule's baseline.timezone; nil is UTC). HourlyDays is the
// hourly rollup's effective retention: window days before it read the
// daily tier (0: every day is hourly). Now decides which days those are;
// it is never part of the content.
type Request struct {
	Database     string
	Window       Window
	Location     *time.Location
	PGVersion    string
	Platform     string
	Capabilities Capabilities
	HourlyDays   int
	Now          time.Time
	// MaxBodyBytes bounds the gzip body; 0 is MaxBodyBytes.
	MaxBodyBytes int64
	// Resource is the database's cloud provider resource ("" when no
	// telemetry names it): sage.host_usage_hourly is keyed by it, so
	// without one the hardware and provider facts are unknown.
	Resource string
	// IncludeConditions puts each plan's full document, its literals
	// redacted, in plans.json (evid-02b; off by default).
	IncludeConditions bool
	// IncludeValueSamples reads pg_stats' most common values and histogram
	// bounds into stats.json (evid-02b; off by default).
	IncludeValueSamples bool
	// Timeouts bound each catalog read on the monitored database; zero is
	// catalogread.Default().
	Timeouts catalogread.Timeouts
}

func (r Request) location() *time.Location {
	if r.Location == nil {
		return time.UTC
	}
	return r.Location
}

func (r Request) maxBody() int64 {
	if r.MaxBodyBytes <= 0 {
		return MaxBodyBytes
	}
	return r.MaxBodyBytes
}
