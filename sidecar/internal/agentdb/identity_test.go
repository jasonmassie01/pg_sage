package agentdb

import (
	"errors"
	"testing"
	"time"
)

func TestAgentIdentityAndDeploymentPingToken(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_identity_token"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_identities WHERE agent_id=$1", "agent_token")

	identity, err := st.UpsertAgentIdentity(ctx, AgentIdentityRequest{
		AgentID:     "agent_token",
		TenantID:    "tenant_agentdb_test",
		OwnerID:     "owner_token",
		DisplayName: "Token agent",
		Metadata: map[string]any{
			"framework": "test",
		},
	})
	if err != nil {
		t.Fatalf("UpsertAgentIdentity: %v", err)
	}
	if identity.Status != "active" || identity.Metadata["framework"] != "test" {
		t.Fatalf("identity = %#v", identity)
	}

	identities, err := st.AgentIdentities(ctx)
	if err != nil {
		t.Fatalf("AgentIdentities: %v", err)
	}
	if !hasAgentIdentity(identities, "agent_token") {
		t.Fatalf("identity list missing agent_token: %#v", identities)
	}

	_, err = st.Register(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "tenant_agentdb_test",
		AgentID:       "agent_token",
		IsolationType: "schema",
		LeaseSeconds:  60,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	token, err := st.CreatePingToken(ctx, id, PingTokenRequest{
		AgentID:        "agent_token",
		ExpiresSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("CreatePingToken: %v", err)
	}
	if token.Token == "" || token.TokenHash != "" || token.Scope != "ping" {
		t.Fatalf("token response = %#v", token)
	}
	if _, err := st.ValidatePingToken(ctx, id, "bad-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token err = %v, want ErrNotFound", err)
	}

	validated, err := st.ValidatePingToken(ctx, id, token.Token)
	if err != nil {
		t.Fatalf("ValidatePingToken: %v", err)
	}
	if validated.Token != "" || validated.LastUsedAt == nil {
		t.Fatalf("validated token should hide secret and set last_used_at: %#v", validated)
	}

	pinged, err := st.AgentPing(ctx, id, token.Token, PingRequest{
		Status:  "active",
		Metrics: map[string]any{"heartbeat": true},
	})
	if err != nil {
		t.Fatalf("AgentPing: %v", err)
	}
	if pinged.LastPingAt == nil || !pinged.LeaseExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("agent ping deployment = %#v", pinged)
	}
}

func TestPingTokenLifecycleRedactionRotationAndRevocation(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_identity_token_lifecycle"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_ping_token_failures WHERE deployment_id=$1", id)

	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "tenant_agentdb_test",
		AgentID:       "agent_token_lifecycle",
		IsolationType: "schema",
		LeaseSeconds:  60,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	token, err := st.CreatePingToken(ctx, id, PingTokenRequest{
		AgentID:        "agent_token_lifecycle",
		ExpiresSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("CreatePingToken: %v", err)
	}

	listed, err := st.PingTokens(ctx, id)
	if err != nil {
		t.Fatalf("PingTokens: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("token count = %d, want 1", len(listed))
	}
	if listed[0].Token != "" || listed[0].TokenHash != "" || listed[0].Status != "active" {
		t.Fatalf("listed token should be redacted and active: %#v", listed[0])
	}

	rotated, err := st.RotatePingToken(ctx, id, token.TokenID, PingTokenRequest{
		ExpiresSeconds: 7200,
	})
	if err != nil {
		t.Fatalf("RotatePingToken: %v", err)
	}
	if rotated.Token == "" || rotated.TokenHash != "" {
		t.Fatalf("rotated token should include only one-time secret: %#v", rotated)
	}
	if rotated.RotatedFromTokenID != token.TokenID {
		t.Fatalf("rotated_from_token_id = %s, want %s",
			rotated.RotatedFromTokenID, token.TokenID)
	}
	if _, err := st.ValidatePingToken(ctx, id, token.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token err = %v, want ErrNotFound", err)
	}
	if _, err := st.ValidatePingToken(ctx, id, rotated.Token); err != nil {
		t.Fatalf("rotated token should validate: %v", err)
	}

	revoked, err := st.RevokePingToken(ctx, id, rotated.TokenID, "owner rotated credentials")
	if err != nil {
		t.Fatalf("RevokePingToken: %v", err)
	}
	if revoked.Status != "revoked" || revoked.RevokedAt == nil || revoked.Token != "" {
		t.Fatalf("revoked token should be redacted and revoked: %#v", revoked)
	}
	if _, err := st.ValidatePingToken(ctx, id, rotated.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token err = %v, want ErrNotFound", err)
	}
}

func TestPingTokenFailuresAreAuditedAndRateLimited(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "adb_identity_token_failures"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_ping_token_failures WHERE deployment_id=$1", id)

	_, err := st.Register(ctx, RegisterRequest{
		DeploymentID:  id,
		TenantID:      "tenant_agentdb_test",
		AgentID:       "agent_token_failures",
		IsolationType: "schema",
		LeaseSeconds:  60,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	for i := 0; i < pingTokenFailureLimit-1; i++ {
		if _, err := st.ValidatePingToken(ctx, id, "bad-token"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failure %d err = %v, want ErrNotFound", i, err)
		}
	}
	if _, err := st.ValidatePingToken(ctx, id, "bad-token"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limit err = %v, want ErrRateLimited", err)
	}

	events, err := st.AuditEvents(ctx, id)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	if !hasAuditEvent(events, "ping_token_failed") {
		t.Fatalf("expected ping_token_failed event in %#v", events)
	}
}

func hasAgentIdentity(identities []AgentIdentity, id string) bool {
	for _, identity := range identities {
		if identity.AgentID == id {
			return true
		}
	}
	return false
}

func hasAuditEvent(events []AuditEvent, name string) bool {
	for _, event := range events {
		if event.Event == name {
			return true
		}
	}
	return false
}
