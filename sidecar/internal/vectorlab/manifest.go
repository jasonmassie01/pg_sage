package vectorlab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// Decode accepts one bounded, strictly typed manifest and validates every budget.
func Decode(r io.Reader) (Manifest, error) {
	var m Manifest
	raw, err := io.ReadAll(io.LimitReader(r, 16*1024*1024+1))
	if err != nil || len(raw) > 16*1024*1024 {
		return m, errors.New("read manifest failed or exceeds 16 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, errors.New("decode manifest: expected one valid JSON workload with known fields")
	}
	if err := d.Decode(new(json.RawMessage)); err != io.EOF {
		return m, errors.New("decode manifest: trailing content is not allowed")
	}
	return m, m.Validate()
}

func (m Manifest) Validate() error {
	for _, name := range append([]string{m.Schema, m.Table, m.IDColumn, m.VectorColumn},
		m.FilterColumns...) {
		if !validName(name, 63) {
			return errors.New("invalid or missing SQL identifier")
		}
	}
	if m.Distance != "l2" && m.Distance != "cosine" && m.Distance != "inner_product" {
		return errors.New("distance must be l2, cosine, or inner_product")
	}
	if err := m.validateBudgets(); err != nil {
		return err
	}
	if err := m.validateVariants(); err != nil {
		return err
	}
	return m.validateQueries()
}

func (m Manifest) validateBudgets() error {
	if m.K < 1 || m.K > 100 || m.Repeats < 1 || m.Repeats > 20 ||
		len(m.Queries) < 3 || len(m.Queries) > 200 ||
		len(m.Variants) < 1 || len(m.Variants) > 16 || len(m.FilterColumns) > 8 {
		return errors.New("bounds: k 1..100, repeats 1..20, queries 3..200, variants 1..16, filters <=8")
	}
	if !finite(m.MinRecall) || m.MinRecall <= 0 || m.MinRecall > 1 ||
		!finite(m.MaxP95MS) || m.MaxP95MS <= 0 || m.MaxP95MS > 60000 {
		return errors.New("min_recall must be (0,1] and max_p95_ms must be (0,60000]")
	}
	if m.StatementTimeoutMS < 1 || m.StatementTimeoutMS > 30000 ||
		m.TotalTimeoutMS < 1 || m.TotalTimeoutMS > 600000 ||
		m.StatementTimeoutMS > m.TotalTimeoutMS {
		return errors.New("explicit timeouts required: statement <=30000ms and total <=600000ms")
	}
	if len(m.Queries)*m.Repeats*(len(m.Variants)+1) > 10000 {
		return errors.New("experiment exceeds 10000 measured queries")
	}
	return nil
}

func (m Manifest) validateVariants() error {
	names := make(map[string]bool)
	for _, v := range m.Variants {
		if !validName(v.Name, 80) || names[v.Name] {
			return errors.New("variant names must be unique, nonempty, <=80 bytes")
		}
		names[v.Name] = true
		if err := v.validate(); err != nil {
			return fmt.Errorf("variant %q: %w", v.Name, err)
		}
	}
	return nil
}

func (v Variant) validate() error {
	if v.EFSearch < 1 || v.EFSearch > 1000 {
		return errors.New("ef_search must be 1..1000")
	}
	switch v.IterativeScan {
	case "off", "strict_order", "relaxed_order":
	default:
		return errors.New("iterative_scan must be off, strict_order, or relaxed_order")
	}
	if n := v.maxScanTuples(); n < 1 || n > 1000000 {
		return errors.New("max_scan_tuples must be 1..1000000 (default 20000)")
	}
	if f := v.scanMemMultiplier(); !finite(f) || f < 1 || f > 1000 {
		return errors.New("scan_mem_multiplier must be 1..1000 (default 1)")
	}
	return nil
}

func (m Manifest) validateQueries() error {
	seen := make(map[string]bool)
	dimensions := len(m.Queries[0].Vector)
	if dimensions < 1 || dimensions > maxHalfvecDimensions {
		return errors.New("vector dimensions must be 1..4000 (2000 for vector columns)")
	}
	for i, q := range m.Queries {
		if !validName(q.ID, 80) || seen[q.ID] || len(q.Vector) != dimensions ||
			len(q.Filters) != len(m.FilterColumns) {
			return fmt.Errorf("query %d: invalid ID, dimensions, or filter count", i)
		}
		seen[q.ID] = true
		var nonzero bool
		for _, v := range q.Vector {
			if !finite(v) || math.Abs(v) > math.MaxFloat32 {
				return fmt.Errorf("query %d: vector values must be finite float32", i)
			}
			nonzero = nonzero || float32(v) != 0
		}
		if m.Distance == "cosine" && !nonzero {
			return errors.New("cosine query cannot be zero")
		}
		for _, value := range q.Filters {
			if len(value) > 4096 || strings.ContainsRune(value, 0) {
				return fmt.Errorf("query %d: invalid filter value", i)
			}
		}
	}
	return nil
}

// HNSW indexes vector columns up to 2000 dimensions and halfvec up to 4000.
const (
	maxVectorDimensions  = 2000
	maxHalfvecDimensions = 4000
)

// checkTargetDimensions applies the cap of the column type read from the
// catalog; the manifest never declares the type.
func checkTargetDimensions(vectorType string, dimensions int) error {
	limit := map[string]int{"vector": maxVectorDimensions, "halfvec": maxHalfvecDimensions}
	maxDims, ok := limit[vectorType]
	if !ok {
		return errors.New("vector_column must be a pgvector vector or halfvec column")
	}
	if dimensions < 1 || dimensions > maxDims {
		return fmt.Errorf("%s columns support 1..%d query dimensions; "+
			"use halfvec for up to 4000", vectorType, maxDims)
	}
	return nil
}

func validName(s string, limit int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= limit && !strings.ContainsRune(s, 0)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
