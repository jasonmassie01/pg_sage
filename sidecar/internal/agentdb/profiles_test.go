package agentdb

import "testing"

func TestSizeProfilesPersistAndValidate(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()
	id := "profile_test_custom"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", id)

	_, err := st.UpsertSizeProfile(ctx, SizeProfile{
		ProfileID:         id,
		Provider:          ProviderDatabricksLakebase,
		ProvisioningLevel: LevelInstance,
		Name:              "lakebase custom",
		CPU:               4,
		MemoryGB:          16,
		StorageGB:         64,
		MaxConnections:    200,
		MonthlyBudgetUSD:  120,
		ProviderParams: map[string]any{
			"mode":    "autoscaling_branch",
			"project": "agent-project",
		},
	})
	if err != nil {
		t.Fatalf("UpsertSizeProfile: %v", err)
	}

	profile, err := st.GetSizeProfile(ctx, id)
	if err != nil {
		t.Fatalf("GetSizeProfile: %v", err)
	}
	if profile.Provider != ProviderDatabricksLakebase {
		t.Fatalf("provider = %s", profile.Provider)
	}
	if profile.ProviderParams["mode"] != "autoscaling_branch" {
		t.Fatalf("provider params = %#v", profile.ProviderParams)
	}

	profiles, err := st.ListSizeProfiles(ctx)
	if err != nil {
		t.Fatalf("ListSizeProfiles: %v", err)
	}
	if len(profiles) == 0 {
		t.Fatal("expected default and custom profiles")
	}

	if err := st.DeleteSizeProfile(ctx, id); err != nil {
		t.Fatalf("DeleteSizeProfile: %v", err)
	}
	if _, err := st.GetSizeProfile(ctx, id); err != ErrNotFound {
		t.Fatalf("Get deleted error = %v, want ErrNotFound", err)
	}
}

func TestSizeProfileValidationRejectsInvalidProvider(t *testing.T) {
	st, ctx, pool := requireAgentDB(t)
	defer pool.Close()

	_, err := st.UpsertSizeProfile(ctx, SizeProfile{
		ProfileID:         "bad_profile",
		Provider:          "unknown",
		ProvisioningLevel: LevelSchema,
		Name:              "bad",
	})
	if err != ErrInvalid {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
