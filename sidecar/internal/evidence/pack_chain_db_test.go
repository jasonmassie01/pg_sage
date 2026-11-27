package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// audit-11: an action pack shipped decision.json and policy.json beside a
// chain proof of action_log only, and a window pack's decisions.json had
// no proof at all: someone who rewrote the decision from "high,
// queue_approval" to "safe, execute" left a pack that read chain_ok. The
// decision rows and the approvals of the policy they name are now proved
// in chains.json.

type packProof struct {
	Chain  string `json:"chain"`
	Source string `json:"source"`
	Note   string `json:"note"`
	Links  []struct {
		RowID int64  `json:"row_id"`
		Op    string `json:"op"`
	} `json:"links"`
	Verification *struct {
		Links    int `json:"links"`
		Problems []struct {
			Kind  string `json:"kind"`
			RowID int64  `json:"row_id"`
		} `json:"problems"`
	} `json:"verification"`
}

func packProofOf(t *testing.T, files map[string][]byte, chain string) packProof {
	t.Helper()
	var proofs []packProof
	if err := json.Unmarshal(files["chains.json"], &proofs); err != nil {
		t.Fatalf("chains.json: %v", err)
	}
	for _, p := range proofs {
		if p.Chain == chain {
			return p
		}
	}
	t.Fatalf("chains.json has no %s proof: %s", chain, files["chains.json"])
	return packProof{}
}

func (p packProof) problems(kind string) int {
	n := 0
	if p.Verification != nil {
		for _, pr := range p.Verification.Problems {
			if pr.Kind == kind {
				n++
			}
		}
	}
	return n
}

// seedApproval approves policy v3 of database 41 in pool and returns the
// approval's id.
func seedApproval(t *testing.T, pool *pgxpool.Pool, approver string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.policy_approvals
		(policy_id, approver, approver_user_id, reason) SELECT id, $1, 5, 'looked at it'
		FROM sage.policy WHERE database_id = 41 AND version = 3 RETURNING id`,
		approver).Scan(&id)
	if err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	return id
}

// bypassExec runs sql with the chain triggers off, the way a superuser
// edits rows behind the chain.
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

func approvedAction(t *testing.T, pool *pgxpool.Pool) (action, approval int64) {
	t.Helper()
	hash := seedPolicy(t, pool)
	approval = seedApproval(t, pool, "alice")
	return seedExecutedDecision(t, pool, &hash), approval
}

func TestActionPackProvesDecisionAndApprovals(t *testing.T) {
	pool := freshDB(t, "evidence_chain_action")
	action, approval := approvedAction(t, pool)
	seedApproval(t, pool, "bob")
	m, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
	if !m.ChainOK {
		t.Fatalf("a clean trail reported broken: %s", files["chains.json"])
	}
	if _, ok := files["chain.json"]; !ok {
		t.Fatal("chain.json (the action's own proof) is gone")
	}
	act := packProofOf(t, files, "action_log")
	dec := packProofOf(t, files, "decision")
	appr := packProofOf(t, files, "policy_approvals")
	if act.Verification == nil || len(act.Links) == 0 || act.Source != "database" {
		t.Fatalf("action proof = %+v", act)
	}
	if dec.Verification == nil || len(dec.Links) != 1 || dec.Links[0].Op != "I" ||
		dec.Source != "database" || len(dec.Verification.Problems) != 0 {
		t.Fatalf("decision proof = %+v, want its insert link verified", dec)
	}
	if appr.Verification == nil || len(appr.Links) != 2 || appr.Links[0].RowID != approval ||
		len(appr.Verification.Problems) != 0 {
		t.Fatalf("approval proof = %+v, want both approvals' links verified", appr)
	}
}

// Rewriting the decision's tier fails the pack, naming the edit.
func TestActionPackReportsAnEditedDecision(t *testing.T) {
	pool := freshDB(t, "evidence_chain_edit")
	action, _ := approvedAction(t, pool)
	mustExec(t, pool, `UPDATE sage.decision SET risk_tier = 'read_only'
		WHERE id = (SELECT decision_id FROM sage.action_log WHERE id = $1)`, action)
	m, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
	if m.ChainOK || packProofOf(t, files, "decision").problems("row_edited") != 1 {
		t.Fatalf("an edited decision was not reported: chain_ok %v, %s", m.ChainOK,
			files["chains.json"])
	}
}

// An approval removed behind the chain fails the pack, even though the
// pack can no longer list it.
func TestActionPackReportsADeletedApproval(t *testing.T) {
	pool := freshDB(t, "evidence_chain_approval")
	action, approval := approvedAction(t, pool)
	bypassExec(t, pool, "DELETE FROM sage.policy_approvals WHERE id = $1", approval)
	m, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
	if m.ChainOK || packProofOf(t, files, "policy_approvals").problems("row_missing") != 1 {
		t.Fatalf("a deleted approval was not reported: chain_ok %v, %s", m.ChainOK,
			files["chains.json"])
	}
}

// Fleet: the decision is proved in the monitored database, the approvals
// in the control database that holds them.
func TestFleetActionPackProvesApprovalsInControl(t *testing.T) {
	monitored := freshDB(t, "evidence_chain_monitored")
	control := freshDB(t, "evidence_chain_control")
	hash := seedPolicy(t, control)
	seedApproval(t, control, "alice")
	action := seedExecutedDecision(t, monitored, &hash)
	m, files := buildFrom(t, Sources{DB: monitored, Control: control, DatabaseID: 41},
		Scope{ActionID: action})
	dec := packProofOf(t, files, "decision")
	appr := packProofOf(t, files, "policy_approvals")
	if !m.ChainOK || dec.Source != "database" || appr.Source != "control" ||
		len(appr.Links) != 1 || len(dec.Links) != 1 {
		t.Fatalf("fleet proofs: chain_ok %v decision %+v approvals %+v", m.ChainOK, dec, appr)
	}
}

// An action no decision authorized has nothing to prove: a note, no
// failure.
func TestActionPackWithoutDecisionNotesIt(t *testing.T) {
	pool := freshDB(t, "evidence_chain_nodecision")
	var action int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('vacuum', 'VACUUM t', 'success')
		RETURNING id`).Scan(&action); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	m, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
	dec := packProofOf(t, files, "decision")
	appr := packProofOf(t, files, "policy_approvals")
	if !m.ChainOK || dec.Note == "" || len(dec.Links) != 0 || appr.Note == "" {
		t.Fatalf("no decision: chain_ok %v decision %+v approvals %+v", m.ChainOK, dec, appr)
	}
}

// A window pack proves its decisions and holds the window's approvals with
// their proof; a rewritten decision fails it.
func TestWindowPackProvesDecisionsAndApprovals(t *testing.T) {
	pool := freshDB(t, "evidence_chain_window")
	action, _ := approvedAction(t, pool)
	m, files := buildFrom(t, Sources{DB: pool}, lastHour())
	dec := packProofOf(t, files, "decision")
	appr := packProofOf(t, files, "policy_approvals")
	if !m.ChainOK || dec.Verification == nil || dec.Verification.Links != 1 ||
		appr.Verification == nil || appr.Verification.Links != 1 {
		t.Fatalf("window proofs: chain_ok %v decision %+v approvals %+v", m.ChainOK, dec,
			appr)
	}
	if e := entry(t, m, "policy_approvals.json"); e.Rows == nil || *e.Rows != 1 ||
		!strings.Contains(string(files["policy_approvals.json"]), "alice") {
		t.Fatalf("policy_approvals.json = %s (%+v)", files["policy_approvals.json"], e)
	}
	mustExec(t, pool, `UPDATE sage.decision SET risk_tier = 'read_only'
		WHERE id = (SELECT decision_id FROM sage.action_log WHERE id = $1)`, action)
	m, files = buildFrom(t, Sources{DB: pool}, lastHour())
	if m.ChainOK || packProofOf(t, files, "decision").problems("row_edited") != 1 {
		t.Fatalf("an edited decision in a window: chain_ok %v, %s", m.ChainOK,
			files["chains.json"])
	}
}
