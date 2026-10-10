package safetybench

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// readonlyFS embeds the read-only corpus fixtures. Each case is a pair of
// files under testdata/readonly:
//
//	<id>.json  — metadata: {"id","technique","expect","self_check"}
//	<id>.sql   — the single statement under test (the "sql" field is loaded
//	             from here, never stored in the JSON, so the fixture SQL is
//	             reviewed as SQL and the metadata stays an attack-free index)
//
// The coordinator authors RO-01.json/.sql .. RO-16.json/.sql here. The
// self-check cases (sc-*) ship now to prove the harness.
//
//go:embed testdata/readonly/*.json testdata/readonly/*.sql
var readonlyFS embed.FS

const readonlyDir = "testdata/readonly"

// caseMeta is the on-disk metadata for a corpus case (the SQL lives beside
// it in <id>.sql).
type caseMeta struct {
	ID        string       `json:"id"`
	Technique string       `json:"technique"`
	Expect    RefusalClass `json:"expect"`
	SelfCheck bool         `json:"self_check,omitempty"`
}

// LoadCases reads every <id>.json/<id>.sql pair under testdata/readonly,
// sorted by id. onlySelfCheck restricts the set to the harness self-checks.
func LoadCases(onlySelfCheck bool) ([]Case, error) {
	return loadCasesFS(readonlyFS, onlySelfCheck)
}

func loadCasesFS(fsys fs.FS, onlySelfCheck bool) ([]Case, error) {
	entries, err := fs.ReadDir(fsys, readonlyDir)
	if err != nil {
		return nil, fmt.Errorf("read corpus dir: %w", err)
	}
	ids := metaIDs(entries)
	cases := make([]Case, 0, len(ids))
	for _, id := range ids {
		c, err := loadOne(fsys, id)
		if err != nil {
			return nil, err
		}
		if onlySelfCheck && !c.SelfCheck {
			continue
		}
		cases = append(cases, c)
	}
	return cases, nil
}

func metaIDs(entries []fs.DirEntry) []string {
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids
}

func loadOne(fsys fs.FS, id string) (Case, error) {
	metaRaw, err := fs.ReadFile(fsys, path.Join(readonlyDir, id+".json"))
	if err != nil {
		return Case{}, fmt.Errorf("read %s.json: %w", id, err)
	}
	var m caseMeta
	if err := json.Unmarshal(metaRaw, &m); err != nil {
		return Case{}, fmt.Errorf("parse %s.json: %w", id, err)
	}
	if m.ID != id {
		return Case{}, fmt.Errorf("%s.json: id %q does not match file name", id, m.ID)
	}
	sqlRaw, err := fs.ReadFile(fsys, path.Join(readonlyDir, id+".sql"))
	if err != nil {
		return Case{}, fmt.Errorf("read %s.sql: %w", id, err)
	}
	sql := strings.TrimSpace(string(sqlRaw))
	if sql == "" {
		return Case{}, fmt.Errorf("%s.sql is empty", id)
	}
	if m.Expect == "" {
		return Case{}, fmt.Errorf("%s.json: expect is required", id)
	}
	return Case{ID: m.ID, Technique: m.Technique, SQL: sql, Expect: m.Expect,
		SelfCheck: m.SelfCheck}, nil
}
