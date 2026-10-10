package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/config"
)

// The kill reaches only configured replicas, by the DSN in each entry's
// environment variable (spec §6.10 step 6, GR-01). An unset variable is
// still a configured replica: the kill reports it unreachable instead of
// silently skipping it.
func TestKillReplicasFromEnv(t *testing.T) {
	env := map[string]string{"R1_DSN": "postgres://u:p@r1/db"}
	getenv := func(k string) string { return env[k] }
	got := killReplicas([]config.DatabaseReplica{{Name: "r1", DSNEnv: "R1_DSN"},
		{Name: "r2", DSNEnv: "R2_DSN"}}, getenv)
	if len(got) != 2 {
		t.Fatalf("replicas = %+v", got)
	}
	if got[0] != (agentguard.Replica{Name: "r1", DSN: "postgres://u:p@r1/db"}) {
		t.Fatalf("r1 = %+v", got[0])
	}
	if got[1].Name != "r2" || got[1].DSN != "" {
		t.Fatalf("r2 = %+v", got[1])
	}
	if len(killReplicas(nil, getenv)) != 0 {
		t.Fatal("no replicas configured must yield none")
	}
}

func TestKillConfigFromSettings(t *testing.T) {
	c := config.DefaultConfig()
	c.Agents.KillVerifyTimeoutSeconds = 4
	c.Agents.SingleOperatorMode = true
	c.Agents.Roles.StatementTimeoutMS = 5000
	kc := killConfig(c)
	if kc.VerifyTimeout != 4*time.Second || !kc.SingleOperatorMode ||
		kc.Roles.StatementTimeout != 5*time.Second {
		t.Fatalf("kill config = %+v", kc)
	}
	if err := kc.Validate(); err != nil {
		t.Fatalf("derived config invalid: %v", err)
	}
	if def := killConfig(nil); def.VerifyTimeout != 10*time.Second {
		t.Fatalf("nil config = %+v", def)
	}
}
