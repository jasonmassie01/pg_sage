// Fails the run before any test when the full-surface fixture is not set up.
// A missing fixture is a broken run, never a reason to skip a test (ci-08).
const required = [
  'PG_SAGE_E2E_FIXTURE', 'PG_SAGE_E2E_BASE_URL', 'PG_SAGE_E2E_METRICS_URL',
  'PG_SAGE_E2E_PG_CONTAINER', 'PG_SAGE_E2E_PG_PORT', 'PG_SAGE_ADMIN_EMAIL', 'PG_SAGE_ADMIN_PASS',
];

export default function globalSetup(): void {
  const missing = required.filter((name) => !process.env[name]);
  if (process.env.PG_SAGE_E2E_FIXTURE && process.env.PG_SAGE_E2E_FIXTURE !== 'full-surface') {
    missing.push(`PG_SAGE_E2E_FIXTURE=full-surface (got ${process.env.PG_SAGE_E2E_FIXTURE})`);
  }
  if (missing.length > 0) {
    throw new Error('The root e2e suite needs the disposable full-surface fixture. Run '
      + '`bash scripts/ci/full-surface-fixture.sh 17` and load the settings it writes. '
      + `Missing: ${missing.join(', ')}`);
  }
}
