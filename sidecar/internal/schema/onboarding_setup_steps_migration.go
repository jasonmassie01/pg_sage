package schema

// ddlOnboardingSetupSteps (ux-04b) records when the setup wizard's model,
// notification channel and autonomy steps were first finished for each
// database. NULL is "not yet"; pg_sage_setup_within_hour_ratio reads them
// against installed_at.
const ddlOnboardingSetupSteps = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.onboarding ADD COLUMN IF NOT EXISTS model_configured_at timestamptz;
ALTER TABLE sage.onboarding ADD COLUMN IF NOT EXISTS channel_configured_at timestamptz;
ALTER TABLE sage.onboarding ADD COLUMN IF NOT EXISTS autonomy_chosen_at timestamptz;
`
