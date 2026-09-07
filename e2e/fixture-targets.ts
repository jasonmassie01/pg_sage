// Disposable local fixtures only. Never point these tests at a monitored user database.
export const fixtureTargets = [
  { name: 'testdb', container: 'pgsage_audit_20260904', db: 'audit_browser_target1' },
  { name: 'testdb2', container: 'pgsage_audit_20260904', db: 'audit_browser_target2' },
  { name: 'health_test', container: 'pgsage_audit_20260904', db: 'audit_browser_target3' },
];
