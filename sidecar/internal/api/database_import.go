package api

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/pg-sage/sidecar/internal/store"
)

// csvImportResult is the response for CSV bulk import.
type csvImportResult struct {
	Imported int              `json:"imported"`
	Skipped  int              `json:"skipped"`
	Errors   []csvImportError `json:"errors"`
}

type csvImportError struct {
	Row   int    `json:"row"`
	Error string `json:"error"`
}

// processCSVImport reads a CSV file and creates database records.
// CSV columns: name,host,port,database_name,username,password,sslmode
func processCSVImport(
	ctx context.Context,
	ds *store.DatabaseStore,
	file io.Reader,
	createdBy int,
) csvImportResult {
	result := csvImportResult{
		Errors: []csvImportError{},
	}
	reader := csv.NewReader(file)

	// Read and validate header row.
	header, err := reader.Read()
	if err != nil {
		result.Errors = append(result.Errors, csvImportError{
			Row: 1, Error: "failed to read CSV header",
		})
		return result
	}
	if !validCSVHeader(header) {
		result.Errors = append(result.Errors, csvImportError{
			Row: 1, Error: "invalid CSV header: expected " +
				"name,host,port,database_name," +
				"username,password,sslmode",
		})
		return result
	}

	count, _ := ds.Count(ctx)
	row := 1
	for {
		row++
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			result.Errors = append(result.Errors,
				csvImportError{
					Row:   row,
					Error: fmt.Sprintf("parse error: %v", readErr),
				})
			continue
		}
		importErr := importOneRow(
			ctx, ds, record, row, createdBy, &count)
		if importErr != nil {
			result.Errors = append(result.Errors, *importErr)
			continue
		}
		result.Imported++
	}
	result.Skipped = len(result.Errors)
	return result
}

func importOneRow(
	ctx context.Context,
	ds *store.DatabaseStore,
	record []string,
	row, createdBy int,
	count *int,
) *csvImportError {
	if len(record) < 7 {
		return &csvImportError{
			Row:   row,
			Error: "not enough columns (need 7)",
		}
	}
	if *count >= 50 {
		return &csvImportError{
			Row:   row,
			Error: "maximum of 50 databases reached",
		}
	}
	port, err := strconv.Atoi(record[2])
	if err != nil {
		return &csvImportError{
			Row:   row,
			Error: fmt.Sprintf("invalid port: %s", record[2]),
		}
	}
	input := store.DatabaseInput{
		Name:          record[0],
		Host:          record[1],
		Port:          port,
		DatabaseName:  record[3],
		Username:      record[4],
		Password:      record[5],
		SSLMode:       record[6],
		TrustLevel:    "observation",
		ExecutionMode: "approval",
	}
	_, err = ds.Create(ctx, input, createdBy)
	if err != nil {
		msg := err.Error()
		if !errors.Is(err, store.ErrValidation) {
			// Internal failure (DB / encryption / marshal).
			// Log the real error and return a generic row-level
			// message so we don't leak PG or crypto internals via
			// the import response.
			slog.Error("csv import row create failed",
				"row", row, "err", err)
			msg = "internal error creating row"
		}
		return &csvImportError{
			Row:   row,
			Error: msg,
		}
	}
	*count++
	return nil
}

func validCSVHeader(header []string) bool {
	expected := []string{
		"name", "host", "port", "database_name",
		"username", "password", "sslmode",
	}
	if len(header) < len(expected) {
		return false
	}
	for i, col := range expected {
		if header[i] != col {
			return false
		}
	}
	return true
}
