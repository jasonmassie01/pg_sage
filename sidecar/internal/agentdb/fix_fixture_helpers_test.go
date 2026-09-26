package agentdb

// testDrill is restore-drill evidence for fixtures that need a
// restore_verified backup (G8-B11 requires recorded drill evidence).
func testDrill(backupID string) RestoreDrillRequest {
	return RestoreDrillRequest{BackupID: backupID, EvidenceURI: "test://restore-drill",
		Target: "fixture-restore", Checks: []string{"select 1"}, ActorID: "test"}
}
