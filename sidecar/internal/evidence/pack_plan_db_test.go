package evidence

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// plan-01b: the pack of an action a plan step ran carries the plan's
// events and the model passes it was built from, with their chain proofs,
// so the plan can be audited and replayed from the pack alone.

// No concurrent access tests: a pack build is a read of committed rows;
// the chain writers' ordering is covered in auditchain and changeplan.

var packPlanHash = strings.Repeat("c3", 32)

const packFenced = "```json\n{\"goal\": \"東京 ✓\"}\n```"

// seedPlanStep links action to step 3 of plan 77 through its queue item's
// identity key, and records two events and one model pass of plan 77 and
// one event of plan 78.
func seedPlanStep(t *testing.T, pool *pgxpool.Pool, action int64, key string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.action_queue (proposed_sql, action_risk, status,
		identity_key, action_log_id) VALUES ('CREATE INDEX x', 'safe', 'executed', $1, $2)`,
		key, action)
	for _, ev := range []struct {
		plan int64
		kind string
	}{{77, "approved"}, {77, "step_started"}, {78, "created"}} {
		mustExec(t, pool, `INSERT INTO sage.plan_event (plan_id, step_id, kind, plan_hash,
			actor, detail) VALUES ($1, 3, $2, $3, 'pg_sage', '{"n": 1}')`, ev.plan, ev.kind,
			packPlanHash)
	}
	mustExec(t, pool, `INSERT INTO sage.plan_model_record (plan_id, model, prompt_version,
		raw_output, raw_output_sha256) VALUES (77, 'model-x', 'plan-v1', $1,
		encode(sha256(convert_to($1, 'UTF8')), 'hex'))`, packFenced)
}

func planProofs(t *testing.T, files map[string][]byte) map[string]packProof {
	t.Helper()
	var proofs []packProof
	if err := json.Unmarshal(files["plan_chains.json"], &proofs); err != nil {
		t.Fatalf("plan_chains.json: %v (%s)", err, files["plan_chains.json"])
	}
	out := map[string]packProof{}
	for _, p := range proofs {
		out[p.Chain] = p
	}
	return out
}

// CHECK-07: the signed pack of a plan step's action holds the plan's
// events and model records with clean chain proofs, and verifies.
func TestActionPackHoldsItsPlansEventsAndModelRecords(t *testing.T) {
	pool := freshDB(t, "evidence_plan")
	action := seedAction(t, pool)
	seedPlanStep(t, pool, action, "plan_step:77:3")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw := build(t, pool, Scope{Database: "orders", ActionID: action}, priv)
	m, err := Verify(bytes.NewReader(raw), pub)
	if err != nil || !m.ChainOK {
		t.Fatalf("verify the plan step's pack: %+v %v", m, err)
	}
	files := readPack(t, raw)
	var events []map[string]any
	if err := json.Unmarshal(files["plan_events.json"], &events); err != nil {
		t.Fatalf("plan_events.json: %v (%s)", err, files["plan_events.json"])
	}
	if len(events) != 2 || events[0]["kind"] != "approved" ||
		events[1]["kind"] != "step_started" || events[0]["plan_id"] != float64(77) {
		t.Fatalf("plan events = %v, want plan 77's two", events)
	}
	var records []map[string]any
	if err := json.Unmarshal(files["plan_model_records.json"], &records); err != nil {
		t.Fatalf("plan_model_records.json: %v", err)
	}
	if len(records) != 1 || records[0]["raw_output"] != packFenced ||
		records[0]["model"] != "model-x" {
		t.Fatalf("model records = %v, want the fenced output byte for byte", records)
	}
	proofs := planProofs(t, files)
	ev, rec := proofs["plan_events"], proofs["plan_model_records"]
	if ev.Verification == nil || len(ev.Links) != 2 || ev.problems("row_edited") != 0 ||
		len(ev.Verification.Problems) != 0 {
		t.Fatalf("plan events proof = %+v", ev)
	}
	if rec.Verification == nil || len(rec.Links) != 1 || len(rec.Verification.Problems) != 0 {
		t.Fatalf("model records proof = %+v", rec)
	}
	if e := entry(t, m, "plan_events.json"); e.Rows == nil || *e.Rows != 2 {
		t.Fatalf("manifest entry of plan_events.json = %+v", e)
	}
}

// A plan event edited behind the chain fails the pack, naming the edit.
func TestActionPackReportsAnEditedPlanEvent(t *testing.T) {
	pool := freshDB(t, "evidence_plan_edit")
	action := seedAction(t, pool)
	seedPlanStep(t, pool, action, "plan_step:77:3")
	bypassExec(t, pool, `UPDATE sage.plan_event SET actor = 'mallory'
		WHERE plan_id = 77 AND kind = 'approved'`)
	m, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
	if m.ChainOK {
		t.Fatalf("an edited plan event left chain_ok: %s", files["plan_chains.json"])
	}
	if ev := planProofs(t, files)["plan_events"]; ev.problems("row_edited") != 1 {
		t.Fatalf("plan events proof = %+v, want one row_edited", ev)
	}
}

// An action no plan step ran, or whose identity key only looks like a
// plan step's, has no plan files, and its pack is otherwise unchanged.
func TestActionPackWithoutAPlanStepHasNoPlanFiles(t *testing.T) {
	for i, key := range []string{"", "plan_step:x:3", "plan_step:77", "index:public.t"} {
		pool := freshDB(t, fmt.Sprintf("evidence_noplan_%d", i))
		action := seedAction(t, pool)
		if key != "" {
			seedPlanStep(t, pool, action, key)
		}
		_, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
		for _, name := range []string{"plan_events.json", "plan_model_records.json",
			"plan_chains.json"} {
			if _, ok := files[name]; ok {
				t.Fatalf("key %q: pack has %s", key, name)
			}
		}
		if _, ok := files["chains.json"]; !ok {
			t.Fatalf("key %q: chains.json is gone", key)
		}
	}
}
