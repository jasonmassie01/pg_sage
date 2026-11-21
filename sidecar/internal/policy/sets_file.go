package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// SetFile is a set's file form (YAML, fleetpol-01a): its selectors,
// priority and requirements, and its document with the version it was
// exported at. The API's import and the GitOps directory (cli-02a) both
// read it with DecodeSetFile, so there is one parser.
type SetFile struct {
	Name         string           `json:"name"`
	Priority     int              `json:"priority"`
	Selectors    []Selector       `json:"selectors"`
	Requirements SetRequirements  `json:"requirements"`
	Document     *SetFileDocument `json:"document,omitempty"`
}

// SetFileDocument is a set file's document: its profile and body, and the
// version it was exported at (0 when the file does not say).
type SetFileDocument struct {
	Version int64           `json:"version"`
	Profile string          `json:"profile"`
	Body    json.RawMessage `json:"body"`
}

// ErrNotYAML is a set file that does not parse as YAML.
var ErrNotYAML = errors.New("policy set file is not YAML")

// Set is the file's set.
func (f SetFile) Set() PolicySet {
	return PolicySet{Name: f.Name, Priority: f.Priority, Selectors: f.Selectors,
		Requirements: f.Requirements}
}

// DecodeSetFile reads a set file (unknown keys refused) and validates its
// set against the review rubric's dimensions. The document is checked
// where it is used: a proposal parses it.
func DecodeSetFile(raw []byte, dimensions []string) (SetFile, error) {
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return SetFile{}, fmt.Errorf("%w: %v", ErrNotYAML, err)
	}
	return DecodeSetTree(tree, dimensions)
}

// DecodeSetTree is DecodeSetFile for a file already parsed into a tree.
func DecodeSetTree(tree any, dimensions []string) (SetFile, error) {
	asJSON, err := json.Marshal(tree)
	var file SetFile
	if err == nil {
		err = DecodeStrictJSON(asJSON, &file)
	}
	if err != nil {
		return SetFile{}, fmt.Errorf("%w: invalid policy set file: %v", ErrInvalidSet, err)
	}
	if err := ValidateSet(file.Set(), dimensions); err != nil {
		return SetFile{}, err
	}
	return file, nil
}

// EncodeSetFile writes set's file form through JSON, so YAML keys are the
// API's JSON names.
func EncodeSetFile(set SetDocument) ([]byte, error) {
	file := SetFile{Name: set.Set.Name, Priority: set.Set.Priority,
		Selectors: set.Set.Selectors, Requirements: set.Set.Requirements}
	if set.Policy != nil {
		file.Document = &SetFileDocument{Version: set.Policy.Version,
			Profile: set.Policy.Profile, Body: set.Policy.Document}
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("encode policy set %s: %w", set.Set.Name, err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("encode policy set %s: %w", set.Set.Name, err)
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(tree); err != nil {
		return nil, fmt.Errorf("encode policy set %s as YAML: %w", set.Set.Name, err)
	}
	return out.Bytes(), encoder.Close()
}
