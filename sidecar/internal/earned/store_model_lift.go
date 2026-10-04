package earned

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Set-based read of the newest model-lift measurement per family
// (roadmap 2.4): the deployment's newest bench report counting for the
// running build (the same build rule as promotion evidence) whose model
// lift has a held-out record of the model arm for the family. A live
// measurement wins over a fake-model one (the release bench's replay
// carries a fake-model lift, which never earns authority), then the
// newest.
const modelLiftSetSQL = `/* pg_sage */ SELECT r.*, f.family FROM unnest($4::text[]) AS f(family)
CROSS JOIN LATERAL (` + evalRunSelect + `
	 WHERE deployment_id = $1 AND source = 'bench' AND model_lift IS NOT NULL
	   AND model_lift->'records' @> jsonb_build_array(jsonb_build_object(
	       'family', f.family, 'arm', $5::text, 'split', 'held_out'))
	   AND ((pg_sage_version = '' AND pg_sage_commit = '')
	     OR ($2 <> '' AND pg_sage_commit = $2)
	     OR (($2 = '' OR pg_sage_commit = '') AND $3 <> '' AND pg_sage_version = $3))
	 ORDER BY (model_lift->>'llm_mode' = 'live') DESC, generated_at DESC, ingested_at DESC
	 LIMIT 1) r`

// modelLiftSet is, per family, its newest measurement (absent: none).
func (s *PostgresStore) modelLiftSet(ctx context.Context, families []Family) (
	map[Family]*EvalRun, error) {
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, string(f))
	}
	b := s.runningBuild()
	out := map[Family]*EvalRun{}
	err := s.queryEach(ctx, "read model lift", modelLiftSetSQL,
		[]any{s.deployment, b.Commit, b.Version, names, ModelArm}, func(rows pgx.Rows) error {
			var f string
			run, err := s.scanEvalRun(trailingRow{Row: rows, extra: &f})
			if err != nil {
				return err
			}
			out[Family(f)] = &run
			return nil
		})
	return out, err
}
