// Package vectorlab measures filtered HNSW recall without changing database state.
package vectorlab

import "time"

type Manifest struct {
	Schema             string    `json:"schema"`
	Table              string    `json:"table"`
	IDColumn           string    `json:"id_column"`
	VectorColumn       string    `json:"vector_column"`
	FilterColumns      []string  `json:"filter_columns,omitempty"`
	Distance           string    `json:"distance"`
	K                  int       `json:"k"`
	Repeats            int       `json:"repeats"`
	MinRecall          float64   `json:"min_recall"`
	MaxP95MS           float64   `json:"max_p95_ms"`
	StatementTimeoutMS int       `json:"statement_timeout_ms"`
	TotalTimeoutMS     int       `json:"total_timeout_ms"`
	Variants           []Variant `json:"variants"`
	Queries            []Query   `json:"queries"`
}

type Variant struct {
	Name          string `json:"name"`
	EFSearch      int    `json:"ef_search"`
	IterativeScan string `json:"iterative_scan"`
}

type Query struct {
	ID      string    `json:"id"`
	Vector  []float64 `json:"vector"`
	Filters []string  `json:"filters,omitempty"`
}

type Report struct {
	FormatVersion   int             `json:"format_version"`
	ManifestSHA256  string          `json:"manifest_sha256"`
	StartedAt       time.Time       `json:"started_at"`
	PostgresVersion string          `json:"postgres_version"`
	VectorVersion   string          `json:"vector_version"`
	Snapshot        string          `json:"snapshot"`
	QueryCount      int             `json:"query_count"`
	Repeats         int             `json:"repeats"`
	ExactP95MS      float64         `json:"exact_p95_ms"`
	Variants        []VariantResult `json:"variants"`
	Recommendation  string          `json:"recommendation,omitempty"`
	AutoApply       bool            `json:"auto_apply"`
	Scope           string          `json:"scope"`
}

type VariantResult struct {
	Variant   Variant       `json:"settings"`
	MinRecall float64       `json:"min_recall"`
	P95MS     float64       `json:"p95_ms"`
	Qualified bool          `json:"qualified"`
	Reasons   []string      `json:"reasons"`
	Queries   []QueryResult `json:"queries"`
}

type QueryResult struct {
	QueryID     string   `json:"query_id"`
	MinRecall   float64  `json:"min_recall"`
	Underfilled int      `json:"underfilled_trials"`
	TruthCount  int      `json:"truth_count"`
	Ambiguous   bool     `json:"ambiguous_boundary_tie"`
	IndexNames  []string `json:"hnsw_indexes"`
}

type row struct {
	id       string
	distance float64
}

type observation struct {
	rows    []row
	elapsed time.Duration
	indexes []string
}
