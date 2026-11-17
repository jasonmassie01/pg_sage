// Package pressure answers check_pressure (guard-01a): whether a backfill
// or bulk write on a monitored database should proceed, slow down (with a
// batch size and a sleep) or stop, from what pg_sage can read about the
// server right now: replica replay lag, transaction and multixact ID age
// against the freeze maximum, a vacuum on the target table, the WAL rate,
// the lock queue on the target table and free storage. It only reads, in
// bounded read-only transactions, and never acts: a caller outside
// pg_sage asks, the caller decides.
package pressure

import (
	"errors"
	"time"
)

// Kind is the answer.
type Kind string

const (
	Proceed Kind = "proceed"
	Slow    Kind = "slow"
	Stop    Kind = "stop"
)

// State is what is known about one input.
type State string

const (
	StateKnown State = "known"
	// StateUnknown is an input that could not be read (the reason says
	// why); it never makes the answer proceed on its own account.
	StateUnknown State = "unknown"
	// StateNotApplicable is an input that does not exist here, such as the
	// replica lag of a server without replicas.
	StateNotApplicable State = "not_applicable"
)

// Band is where a known input's value sits against its limits.
type Band string

const (
	BandOK   Band = "ok"
	BandSlow Band = "slow"
	BandStop Band = "stop"
)

var (
	// ErrUnknownTable is a table name to_regclass does not resolve.
	ErrUnknownTable = errors.New("table not found")
	// ErrInvalidRequest is a request the checker refuses before reading.
	ErrInvalidRequest = errors.New("invalid pressure request")
	// ErrUnavailable is a checker that cannot read (none built, no source).
	ErrUnavailable = errors.New("pressure check unavailable")
)

// Request is one check: the database (informational: a checker reads one
// database), the target table ("" for none: the table inputs are then not
// applicable), the caller's job name and its current batch size (0 for
// none: the answer then carries the batch factor only).
type Request struct {
	Database string
	Table    string
	Job      string
	Batch    int
}

// Input is one input in an answer: its value, its limits, where it sits
// and how fresh it is.
type Input struct {
	Name       string     `json:"name"`
	State      State      `json:"state"`
	Value      *float64   `json:"value"`
	Unit       string     `json:"unit"`
	SlowLimit  *float64   `json:"slow_limit"`
	StopLimit  *float64   `json:"stop_limit"`
	Band       Band       `json:"band,omitempty"`
	Required   bool       `json:"required"`
	Reason     string     `json:"reason,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	AgeSeconds *float64   `json:"age_seconds,omitempty"`
}

// Verdict is the answer to one check.
type Verdict struct {
	Database string `json:"database"`
	Table    string `json:"table,omitempty"`
	Job      string `json:"job,omitempty"`
	Verdict  Kind   `json:"verdict"`
	// BatchFactor is what to multiply the batch by: 1 to proceed, halved
	// for each input in its slow band, 0 to stop.
	BatchFactor float64 `json:"batch_factor"`
	// SuggestedBatch is the request's batch times BatchFactor (at least 1
	// when slowing); 0 when the request named no batch or the answer stops.
	SuggestedBatch int `json:"suggested_batch,omitempty"`
	// SleepSeconds is how long to wait before the next batch (or, after a
	// stop, before asking again).
	SleepSeconds float64   `json:"sleep_seconds"`
	Reasons      []string  `json:"reasons"`
	Inputs       []Input   `json:"inputs"`
	CheckedAt    time.Time `json:"checked_at"`
	CachedUntil  time.Time `json:"cached_until"`
}

// Reading is one input as a source read it.
type Reading struct {
	State  State
	Value  float64
	Reason string
	At     time.Time
}

// Known is a read value observed at at.
func Known(v float64, at time.Time) Reading {
	return Reading{State: StateKnown, Value: v, At: at}
}

// Unknown is an input that could not be read, and why.
func Unknown(reason string) Reading { return Reading{State: StateUnknown, Reason: reason} }

// NotApplicable is an input that does not exist here, and why.
func NotApplicable(reason string) Reading {
	return Reading{State: StateNotApplicable, Reason: reason}
}

// Readings are one read set of a source: the SQL inputs by name, the WAL
// insert position in bytes (the checker turns positions into a rate), the
// slowest replica's replay position in bytes (not applicable when none is
// behind) and the server's freeze maxima (0 when unread).
type Readings struct {
	Inputs         map[string]Reading
	WALPosition    Reading
	ReplayPosition Reading
	XIDFreezeMax   float64
	MXIDFreezeMax  float64
}
