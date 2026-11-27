package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"sort"

	"github.com/pg-sage/sidecar/internal/migration"
)

// SARIF 2.1.0 (OASIS), the subset code scanning reads.

const (
	sarifVersion = "2.1.0"
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifInfoURI = "https://github.com/jasonmassie01/pg_sage/blob/master/docs/migration-ci.md"
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"` // error | warning | note
	Message   sarifMessage    `json:"message"`
	Locations []sarifLocation `json:"locations"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           sarifRegion   `json:"region"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

func sarifLevel(verdict string) string {
	switch verdict {
	case migration.VerdictRisky:
		return "error"
	case migration.VerdictReview:
		return "warning"
	}
	return "note"
}

func buildSARIF(file string, rep migration.Report) sarifLog {
	uri := filepath.ToSlash(file)
	rules := map[string]string{}
	results := []sarifResult{}
	for _, f := range lintFindings(rep) {
		if _, ok := rules[f.rule]; !ok {
			rules[f.rule] = "pg_sage DDL safety rule " + f.rule
		}
		results = append(results, sarifResult{RuleID: f.rule, Level: sarifLevel(f.verdict),
			Message: sarifMessage{Text: f.message},
			Locations: []sarifLocation{{PhysicalLocation: sarifPhysical{
				ArtifactLocation: sarifArtifact{URI: uri},
				Region:           sarifRegion{StartLine: f.line}}}}})
	}
	driver := sarifDriver{Name: "pg_sage", Version: version, InformationURI: sarifInfoURI,
		Rules: []sarifRule{}}
	for id, desc := range rules {
		driver.Rules = append(driver.Rules, sarifRule{ID: id,
			ShortDescription: sarifMessage{Text: desc}})
	}
	sort.Slice(driver.Rules, func(i, j int) bool { return driver.Rules[i].ID < driver.Rules[j].ID })
	return sarifLog{Schema: sarifSchema, Version: sarifVersion,
		Runs: []sarifRun{{Tool: sarifTool{Driver: driver}, Results: results}}}
}

func writeSARIF(w io.Writer, file string, rep migration.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(buildSARIF(file, rep))
}
