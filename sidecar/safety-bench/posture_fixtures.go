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

// PostureScenarios returns the v0 posture scenarios with their setup SQL
// loaded. The scenarios cover the §11 v0 set: a Supabase-style exposed
// table (AP-03), a permissive policy (AP-04), a SECURITY DEFINER function
// (AP-05), PUBLIC CREATE on a schema (AP-07), and the pgvector-version arm
// (AP-10), which is a catalog fact the real provider reads rather than a
// posture the bench can create, so it carries a version note and no SQL.
func PostureScenarios() ([]PostureScenario, error) {
	decls := postureDecls()
	out := make([]PostureScenario, 0, len(decls))
	for _, d := range decls {
		if d.file != "" {
			sql, err := fs.ReadFile(postureFS, path.Join(postureDir, d.file))
			if err != nil {
				return nil, fmt.Errorf("read posture fixture %s: %w", d.file, err)
			}
			d.scenario.SetupSQL = strings.TrimSpace(string(sql))
		}
		out = append(out, d.scenario)
	}
	return out, nil
}

type postureDecl struct {
	scenario PostureScenario
	file     string
}

func postureDecls() []postureDecl {
	return []postureDecl{
		{PostureScenario{ID: "PS-exposed-table", Name: "Supabase-style exposed table",
			Expect: []string{"AP-03"}}, "exposed_table.sql"},
		{PostureScenario{ID: "PS-permissive-policy", Name: "Permissive RLS policy",
			Expect: []string{"AP-04"}}, "permissive_policy.sql"},
		{PostureScenario{ID: "PS-definer-function", Name: "SECURITY DEFINER function",
			Expect: []string{"AP-05"}}, "definer_function.sql"},
		{PostureScenario{ID: "PS-public-create", Name: "PUBLIC CREATE on schema",
			Expect: []string{"AP-07"}}, "public_create.sql"},
		{PostureScenario{ID: "PS-pgvector-version",
			Name:   "pgvector / server version",
			Expect: []string{"AP-10"},
			VersionNote: "AP-10 reads the installed pgvector and server version from " +
				"the catalog; the bench does not downgrade extensions. The real " +
				"provider reports this arm from catalog facts."},
			""},
	}
}
