// Disposable local fixtures only. Never point these tests at a monitored user database.
// scripts/ci/full-surface-fixture.sh creates the container, the databases and the
// three managed targets, and exports PG_SAGE_E2E_PG_CONTAINER and PG_SAGE_E2E_PG_PORT.
export const fixtureContainer = process.env.PG_SAGE_E2E_PG_CONTAINER || 'pgsage_e2e';
export const fixturePgPort = Number(process.env.PG_SAGE_E2E_PG_PORT || 5455);

export const fixtureTargets = [
  { name: 'testdb', container: fixtureContainer, db: 'e2e_target1' },
  { name: 'testdb2', container: fixtureContainer, db: 'e2e_target2' },
  { name: 'health_test', container: fixtureContainer, db: 'e2e_target3' },
];
