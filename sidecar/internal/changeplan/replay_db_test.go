package changeplan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// plan-01b: the model passes a plan was built from are stored byte for
// byte, chained, and handed back as the replay input only when each still
// matches its own hash and its chain link.

// No LLM failure tests (timeouts, rate limits, malformed JSON): nothing
// here calls a model; raw output is stored as given, whatever it holds.

const sha256Empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// bypassExec runs sql with user triggers disabled, the way someone with
// superuser rights edits rows behind the chain.
func bypassExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatalf("replica role: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func mustRecord(t *testing.T, s *Store, planID int64, m ModelPass) ModelPass {
	t.Helper()
	got, err := s.RecordModelPass(context.Background(), planID, m)
	if err != nil {
		t.Fatalf("record model pass: %v", err)
	}
	return got
}

func mustReplay(t *testing.T, s *Store, planID int64) ReplayInput {
	t.Helper()
	in, err := s.ReplayInput(context.Background(), planID)
	if err != nil {
		t.Fatalf("replay input of plan %d: %v", planID, err)
	}
	return in
}

const fenced = "```json\n{\"goal\": \"cut blocks per call\", \"steps\": [1, 2.50]}\n```\n"

// Happy path and nil/empty: two passes come back in order, byte for byte
// (markdown fences and unicode included), each with its chain link; an
// empty output is stored, hashed and replayed as empty, not as missing.
func TestReplay_PassesRoundTripByteForByte(t *testing.T) {
	pool := planPool(t, "plan_replay_happy")
	s := NewStore(pool)
	p := mustCreate(t, s, threeStepPlan())
	first := mustRecord(t, s, p.ID, ModelPass{PackHash: testPack, Model: "model-x",
		PromptVersion: "plan-v1", RawOutput: fenced})
	second := mustRecord(t, s, p.ID, ModelPass{Model: "model-y", PromptVersion: "plan-v2",
		RawOutput: ""})
	if first.ID <= 0 || first.RawOutputSHA256 != sha256Hex(fenced) || first.PlanID != p.ID ||
		second.RawOutputSHA256 != sha256Empty || first.CreatedAt.IsZero() {
		t.Fatalf("recorded %+v / %+v", first, second)
	}
	in := mustReplay(t, s, p.ID)
	if in.PlanID != p.ID || len(in.Passes) != 2 {
		t.Fatalf("replay input = %+v, want two passes", in)
	}
	a, b := in.Passes[0], in.Passes[1]
	if a.ID != first.ID || a.RawOutput != fenced || a.PackHash != testPack ||
		a.Model != "model-x" || a.PromptVersion != "plan-v1" ||
		a.RawOutputSHA256 != sha256Hex(fenced) {
		t.Fatalf("first pass = %+v", a)
	}
	if b.ID != second.ID || b.RawOutput != "" || b.PackHash != "" ||
		b.RawOutputSHA256 != sha256Empty {
		t.Fatalf("empty pass = %+v, want an empty output with the empty hash", b)
	}
	ctx := context.Background()
	for _, pass := range in.Passes {
		links, err := auditchain.LinksForRow(ctx, pool, "plan_model_records", pass.ID)
		if err != nil || len(links) != 1 || links[0].Seq != pass.ChainSeq ||
			links[0].Hash != pass.ChainHash {
			t.Fatalf("pass %d link %d/%s, chain %+v %v", pass.ID, pass.ChainSeq,
				pass.ChainHash, links, err)
		}
	}
}

// Nil/empty: a plan with no model pass replays as empty; a plan that does
// not exist is not found.
func TestReplay_NoPassIsEmptyAndNoPlanIsNotFound(t *testing.T) {
	s := NewStore(planPool(t, "plan_replay_empty"))
	p := mustCreate(t, s, threeStepPlan())
	in := mustReplay(t, s, p.ID)
	if in.PlanID != p.ID || in.Passes == nil || len(in.Passes) != 0 {
		t.Fatalf("replay of a plan with no pass = %+v, want an empty list", in)
	}
	if _, err := s.ReplayInput(context.Background(), p.ID+99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay of a missing plan: %v, want ErrNotFound", err)
	}
	if _, err := s.RecordModelPass(context.Background(), p.ID+99, ModelPass{Model: "m",
		RawOutput: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record for a missing plan: %v, want ErrNotFound", err)
	}
}

// Boundary: exactly 1 MiB is stored and replayed; one byte more is refused
// before the insert.
func TestReplay_OneMiBIsTheLimit(t *testing.T) {
	pool := planPool(t, "plan_replay_limit")
	s := NewStore(pool)
	p := mustCreate(t, s, threeStepPlan())
	exact := strings.Repeat("é", MaxRawOutputBytes/2) // two bytes each
	mustRecord(t, s, p.ID, ModelPass{Model: "m", RawOutput: exact})
	_, err := s.RecordModelPass(context.Background(), p.ID, ModelPass{Model: "m",
		RawOutput: exact + "x"})
	if !errors.Is(err, ErrRawOutputTooLarge) || !strings.Contains(err.Error(), "1048577") {
		t.Fatalf("1 MiB + 1: %v, want ErrRawOutputTooLarge naming the size", err)
	}
	in := mustReplay(t, s, p.ID)
	if len(in.Passes) != 1 || len(in.Passes[0].RawOutput) != MaxRawOutputBytes ||
		in.Passes[0].RawOutput != exact {
		t.Fatalf("replayed %d passes, want the 1 MiB one intact", len(in.Passes))
	}
}

// Tamper: an edited raw output fails the replay with a hash mismatch; an
// output edited together with its hash fails the chain; a record written
// behind the chain is refused as unchained; a pass deleted behind the
// chain (the plan's only or first one included) or a truncation fails it.
func TestReplay_TamperedRecordsAreRefused(t *testing.T) {
	for _, tc := range replayTampers() {
		t.Run(tc.name, func(t *testing.T) {
			pool := planPool(t, "plan_replay_"+strings.ReplaceAll(tc.name, " ", "_"))
			s := NewStore(pool)
			p := mustCreate(t, s, threeStepPlan())
			rec := mustRecord(t, s, p.ID, ModelPass{Model: "m", RawOutput: fenced})
			mustReplay(t, s, p.ID)
			tc.tamper(t, pool, rec.ID, p.ID)
			_, err := s.ReplayInput(context.Background(), p.ID)
			if !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: %v, want %v naming %q", tc.name, err, tc.is, tc.want)
			}
		})
	}
}

// A pass cannot be deleted; a plan retention purged still replays from
// its records, and a pass deleted behind the chain after the purge still
// fails the replay, found from the plan's first event.
func TestReplay_PurgedPlanReplaysAndAPassCannotBeDeleted(t *testing.T) {
	pool := planPool(t, "plan_replay_purged")
	s := NewStore(pool)
	ctx := context.Background()
	p := mustCreate(t, s, threeStepPlan())
	rec := mustRecord(t, s, p.ID, ModelPass{Model: "m", RawOutput: fenced})
	_, err := pool.Exec(ctx, `DELETE FROM sage.plan_model_record WHERE id = $1`, rec.ID)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete of a model pass: %v, want an append-only refusal", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.plan WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("purge the plan: %v", err)
	}
	in := mustReplay(t, s, p.ID)
	if len(in.Passes) != 1 || in.Passes[0].ID != rec.ID || in.Passes[0].RawOutput != fenced {
		t.Fatalf("replay of the purged plan = %+v, want its one pass", in)
	}
	bypassExec(t, pool, `DELETE FROM sage.plan_model_record WHERE id = $1`, rec.ID)
	_, err = s.ReplayInput(ctx, p.ID)
	if !errors.Is(err, ErrReplayChain) || !strings.Contains(err.Error(), "row_missing") {
		t.Fatalf("replay after the only pass was deleted: %v, want row_missing", err)
	}
}

// replayTamper is one way to tamper with a plan's model passes: the error
// the replay must return and the text it must name.
type replayTamper struct {
	name, want string
	tamper     func(t *testing.T, pool *pgxpool.Pool, id, planID int64)
	is         error
}

func replayTampers() []replayTamper {
	return []replayTamper{
		{"edited output", "hash mismatch", func(t *testing.T, pool *pgxpool.Pool, id,
			_ int64) {
			bypassExec(t, pool, `UPDATE sage.plan_model_record SET raw_output = 'forged'
				WHERE id = $1`, id)
		}, ErrReplayHashMismatch},
		{"edited output and hash", "row_edited", func(t *testing.T, pool *pgxpool.Pool,
			id, _ int64) {
			bypassExec(t, pool, `UPDATE sage.plan_model_record SET raw_output = 'forged',
				raw_output_sha256 = $2 WHERE id = $1`, id, sha256Hex("forged"))
		}, ErrReplayChain},
		{"unchained record", "no chain link", func(t *testing.T, pool *pgxpool.Pool, _,
			planID int64) {
			bypassExec(t, pool, `INSERT INTO sage.plan_model_record (plan_id, model,
				raw_output, raw_output_sha256) VALUES ($1, 'm', 'hidden', $2)`, planID,
				sha256Hex("hidden"))
		}, ErrReplayChain},
		{"deleted later pass", "row_missing", func(t *testing.T, pool *pgxpool.Pool, _,
			planID int64) {
			s := NewStore(pool)
			later := mustRecord(t, s, planID, ModelPass{Model: "m2", RawOutput: "second"})
			bypassExec(t, pool, `DELETE FROM sage.plan_model_record WHERE id = $1`, later.ID)
		}, ErrReplayChain},
		{"deleted only pass", "row_missing", func(t *testing.T, pool *pgxpool.Pool, id,
			_ int64) {
			bypassExec(t, pool, `DELETE FROM sage.plan_model_record WHERE id = $1`, id)
		}, ErrReplayChain},
		{"deleted first pass", "row_missing", func(t *testing.T, pool *pgxpool.Pool, id,
			planID int64) {
			mustRecord(t, NewStore(pool), planID, ModelPass{Model: "m2", RawOutput: "two"})
			bypassExec(t, pool, `DELETE FROM sage.plan_model_record WHERE id = $1`, id)
		}, ErrReplayChain},
		{"truncated records", "1 truncations", func(t *testing.T, pool *pgxpool.Pool, _,
			_ int64) {
			if _, err := pool.Exec(context.Background(),
				`TRUNCATE sage.plan_model_record`); err != nil {
				t.Fatalf("truncate: %v", err)
			}
		}, ErrReplayChain},
	}
}
