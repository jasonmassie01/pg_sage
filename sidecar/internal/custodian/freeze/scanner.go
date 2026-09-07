package freeze

import (
	"context"
	"fmt"
	"time"
)

type Scanner struct {
	catalog DatabaseCatalog
	reader  HorizonReader
	sink    ProposalSink
	options ScannerOptions
}

func NewScanner(
	catalog DatabaseCatalog, reader HorizonReader, sink ProposalSink, options ScannerOptions,
) *Scanner {
	return &Scanner{catalog, reader, sink, options}
}

func (s *Scanner) Scan(ctx context.Context) (ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return ScanResult{}, err
	}
	databases, err := s.catalog.ListDatabases(ctx)
	if err != nil {
		return ScanResult{}, fmt.Errorf("list databases: %w", err)
	}
	result := ScanResult{DatabasesDiscovered: len(databases)}
	proposals := make([]Proposal, 0)
	for _, database := range databases {
		if database.IsTemplate || !database.AllowConnections {
			result.DatabasesSkipped++
			continue
		}
		result.DatabasesScanned++
		samples, readErr := s.reader.ReadHorizons(ctx, database)
		if readErr != nil {
			return result, fmt.Errorf("read horizons for %s: %w", database.Name, readErr)
		}
		for _, sample := range samples {
			assessment, evalErr := EvaluateHorizon(s.now(), sample, s.options.Thresholds)
			if evalErr != nil {
				return result, fmt.Errorf("evaluate %s: %w", database.Name, evalErr)
			}
			if assessment.Proposal.Intent != "" {
				proposals = append(proposals, assessment.Proposal)
			}
		}
	}
	if len(proposals) == 0 {
		return result, nil
	}
	if err := s.sink.Publish(ctx, proposals); err != nil {
		return result, fmt.Errorf("publish freeze proposals: %w", err)
	}
	result.ProposalsPublished = len(proposals)
	return result, nil
}

func (s *Scanner) now() time.Time {
	if s.options.Now != nil {
		return s.options.Now()
	}
	return time.Now()
}
