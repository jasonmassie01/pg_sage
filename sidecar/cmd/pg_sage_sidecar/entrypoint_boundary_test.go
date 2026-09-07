package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVectorLabEntrypointHelpAndInvalidArguments(t *testing.T) {
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = []string{"pg_sage", "vector-lab", "--help"}
	if code := runVectorLab(); code != 0 {
		t.Fatalf("supported CLI help exit=%d", code)
	}
	os.Args = []string{"pg_sage", "vector-lab", "--unknown-option"}
	if code := runVectorLab(); code != 1 {
		t.Fatalf("unsupported CLI arguments exit=%d", code)
	}
}

func TestConfigReloadRejectsMalformedCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(path, []byte("trust: [invalid yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = []string{"pg_sage", "--config", path}
	candidate, err := loadConfigCandidate()
	if err == nil || candidate != nil {
		t.Fatalf("malformed candidate accepted: present=%v err=%v", candidate != nil, err)
	}
}

func TestConfigControlPoolNeverUsesArbitraryFleetMember(t *testing.T) {
	prepareMetaGlobals(t)
	oldMeta, oldPool := globalMetaState, pool
	t.Cleanup(func() { globalMetaState, pool = oldMeta, oldPool })
	metadataPool, standalonePool := &pgxpool.Pool{}, &pgxpool.Pool{}
	globalMetaState, pool = &metaDBState{Pool: metadataPool}, standalonePool
	if configControlPool() != metadataPool {
		t.Fatal("metadata mode did not select its global control owner")
	}
	globalMetaState = nil
	if configControlPool() != nil {
		t.Fatal("fleet without metadata owner selected arbitrary database policy")
	}
	cfg.Mode = "standalone"
	if configControlPool() != standalonePool {
		t.Fatal("standalone mode lost its policy owner")
	}
}
