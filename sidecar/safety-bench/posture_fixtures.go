package safetybench

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// postureFS embeds the posture scenario fixtures. Each scenario's setup SQL
// is a file testdata/posture/<id>.sql. The scenario list (ids, names,
// expected detector ids and any version note) is declared in
// PostureScenarios below so the expectations are reviewed with the code.
//
//go:embed testdata/posture/*.sql
var postureFS embed.FS

const postureDir = "testdata/posture"

// PostureScenarios returns the posture scenarios with their setup and
// teardown SQL loaded. They cover the §11 v0 set: a Supabase-style exposed
// table (AP-03), a permissive policy (AP-04), a SECURITY DEFINER function
// (AP-05), PUBLIC CREATE on a schema (AP-07) and an HNSW index on a
// pgvector without the vacuum fix (AP-10), plus two registered agent roles:
// one that bypasses RLS (AP-01) and one that owns a table (AP-02).
func PostureScenarios() ([]PostureScenario, error) {
	decls := postureDecls()
	out := make([]PostureScenario, 0, len(decls))
	for _, d := range decls {
		var err error
		if d.scenario.SetupSQL, err = readPostureFixture(d.file); err != nil {
			return nil, err
		}
		if d.down != "" {
			if d.scenario.TeardownSQL, err = readPostureFixture(d.down); err != nil {
				return nil, err
			}
		}
		out = append(out, d.scenario)
	}
	return out, nil
}

func readPostureFixture(name string) (string, error) {
	sql, err := fs.ReadFile(postureFS, path.Join(postureDir, name))
	if err != nil {
		return "", fmt.Errorf("read posture fixture %s: %w", name, err)
	}
	return strings.TrimSpace(string(sql)), nil
}

type postureDecl struct {
	scenario   PostureScenario
	file, down string
}

func postureDecls() []postureDecl {
	return []postureDecl{
		{scenario: PostureScenario{ID: "PS-exposed-table",
			Name: "Supabase-style exposed table", Expect: []string{"AP-03"},
			Scope: "sb_ps_exposed."}, file: "exposed_table.sql"},
		{scenario: PostureScenario{ID: "PS-permissive-policy", Name: "Permissive RLS policy",
			Expect: []string{"AP-04"}, Scope: "sb_ps_policy."},
			file: "permissive_policy.sql"},
		{scenario: PostureScenario{ID: "PS-definer-function",
			Name: "SECURITY DEFINER function", Expect: []string{"AP-05"},
			Scope: "sb_ps_definer."}, file: "definer_function.sql"},
		{scenario: PostureScenario{ID: "PS-public-create", Name: "PUBLIC CREATE on schema",
			Expect: []string{"AP-07"}, Scope: "sb_ps_public"}, file: "public_create.sql"},
		{scenario: PostureScenario{ID: "PS-agent-attributes",
			Name: "Agent role that bypasses RLS", Expect: []string{"AP-01"},
			Scope: "sage_agentb_sbattrbypa"},
			file: "agent_attributes.sql", down: "agent_attributes_down.sql"},
		{scenario: PostureScenario{ID: "PS-agent-ownership", Name: "Agent role owns a table",
			Expect: []string{"AP-02"}, Scope: "sage_agentb_sbownstore"},
			file: "agent_ownership.sql", down: "agent_ownership_down.sql"},
		{scenario: PostureScenario{ID: "PS-pgvector-version",
			Name: "pgvector HNSW index without the vacuum fix", Expect: []string{"AP-10"},
			Scope: "vector/hnsw",
			VersionNote: "Fires while the installed pgvector is below 0.8.4; on a " +
				"fixed release (or without pgvector) AP-10 correctly stays quiet."},
			file: "pgvector_hnsw.sql", down: "pgvector_hnsw_down.sql"},
	}
}
