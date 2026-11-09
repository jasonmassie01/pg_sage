// Per-database read errors of the onboarding and first-look endpoints: one
// database that cannot be read is named; the others are still shown.

export function entryErrorText(code, database) {
  switch (code) {
    case 'unreachable':
      return `pg_sage cannot reach the database ${database}; check the connection URL ` +
        'and network.'
    case 'no_runtime':
      return `${database} is not running in pg_sage; check its connection settings ` +
        'and the pg_sage log.'
    default:
      return `pg_sage could not read the setup state of ${database}; the pg_sage log ` +
        'has the cause.'
  }
}
