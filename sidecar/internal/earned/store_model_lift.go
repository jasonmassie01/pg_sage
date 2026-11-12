package earned

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Set-based read of the newest model-lift measurement per family
// (roadmap 2.4): the deployment's newest bench report whose model lift has
// a held-out record of the model arm for the family. Only a signed
// release or local-run report stamped with the running build counts
// (benches-09): an unsigned operator upload or an unstamped row never
// earns model-root authority, unlike promotion evidence. A
// measurement taken with the running model ($6, normalized) wins, so a
// newer report from another model never shadows it (llm-01); then a live
// measurement wins over a fake-model one (the release bench's replay
// carries a fake-model lift, which never earns authority), then the
// newest. A row without llm_model (ingested before llm-01) never matches.
const modelLiftSetSQL = `/* pg_sage */ SELECT r.*, f.family FROM unnest($4::text[]) AS f(family)
CROSS JOIN LATERAL (` + evalRunSelect + `
	 WHERE deployment_id = $1 AND source = 'bench' AND model_lift IS NOT NULL
	   AND origin IN ('signed_release', 'local_run')
	   AND model_lift->'records' @> jsonb_build_array(jsonb_build_object(
	       'family', f.family, 'arm', $5::text, 'split', 'held_out'))
	   AND (($2 <> '' AND pg_sage_commit = $2)
	     OR (($2 = '' OR pg_sage_commit = '') AND $3 <> '' AND pg_sage_version = $3))
	 ORDER BY COALESCE(model_lift->>'llm_model' = $6::text AND $6::text <> '', false) DESC,
	          (model_lift->>'llm_mode' = 'live') DESC, generated_at DESC, ingested_at DESC
	 LIMIT 1) r`

// modelLiftSet is, per family, its measurement for an investigator
// running runningModel (absent: none).
func (s *PostgresStore) modelLiftSet(ctx context.Context, families []Family,
	runningModel string) (map[Family]*EvalRun, error) {
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, string(f))
	}
	b := s.runningBuild()
	out := map[Family]*EvalRun{}
	err := s.queryEach(ctx, "read model lift", modelLiftSetSQL,
		[]any{s.deployment, b.Commit, b.Version, names, ModelArm,
			modellift.NormalizeModel(runningModel)}, func(rows pgx.Rows) error {
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
