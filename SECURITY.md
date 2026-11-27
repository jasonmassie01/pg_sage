# Security Policy

## Supported Versions

Only the latest minor release line receives security fixes. Earlier versions
are end-of-life and will not receive patches.

| Version | Supported          |
| ------- | ------------------ |
| 2.6.x   | Yes                |
| < 2.6   | No (EOL)           |

The C extension (version 0.5.0) was removed from the repository and receives no
security fixes. Its last source is preserved at the `c-extension-final` tag.

When a new minor version is released, update this table in the same change
as the CHANGELOG entry. `TestSecurityDocsSupportedVersionMatchesVERSION`
(`sidecar/internal/config`) fails when the supported row is not the minor in
`VERSION`, and `TestReleaseMetadataAgrees` (`sidecar/internal/docscontract`)
fails when `VERSION`, the newest `## vX.Y.Z` CHANGELOG heading and this table
disagree.

## Reporting a Vulnerability

Please do not report security vulnerabilities through public GitHub Issues,
discussions, or pull requests.

Instead, email `jmass0729@gmail.com` with the subject line
`pg_sage security: <short description>`. Include:

- A description of the issue and its impact
- Steps to reproduce, or a proof-of-concept
- The affected version(s) and configuration, if known
- Any suggested mitigations

You will receive an acknowledgment within 72 hours. We will work with you to
understand, reproduce, and remediate the issue, and will keep you informed of
progress toward a fix and coordinated disclosure.

## Severity Considerations

pg_sage connects as a dedicated non-superuser role. A superuser bypasses
row-level security and holds every agent role, so pg_sage warns at startup when
it connects as one; posture finding AP-14 flags a superuser or `BYPASSRLS` role.
On managed services, `rds_superuser`, `cloudsqlsuperuser`, `azure_pg_admin`
and the like are admin roles to grant pg_sage's privileges from, not roles to
run it as. See [Required Database Grants](docs/security.md#required-database-grants).

Operators grant that role more than read access so pg_sage can act:

- `pg_monitor` and `pg_read_all_stats` to read statistics and activity
- `pg_signal_backend` to cancel or terminate sessions
- `CREATE` on the schemas where it creates objects, and ownership of the
  `sage` schema
- membership in the table owner's role, which `CREATE INDEX`,
  `CREATE STATISTICS` and other table DDL need
- `MAINTAIN` (PostgreSQL 17+) to `VACUUM`, `ANALYZE` and `REINDEX` tables it
  does not own
- `CREATEROLE` (PostgreSQL 16+) when agent governance manages agent roles;
  without it agent governance is posture-only

Through owner-role membership a pg_sage connection holds the privileges of
the application's table owner: it could read, change or drop that owner's
tables. pg_sage confines itself to typed, validated actions behind trust
levels, approval and the emergency stop (see
[What pg_sage can change](docs/security.md#what-pg_sage-can-change)). Any way
past that SQL boundary therefore acts with the owner's privileges, and the
following classes of vulnerability are considered HIGH severity by default:

- Flaws in the executor that could allow unintended SQL to run
- Bypasses of SQL validation, allow-list, or trust-level gating
- Authentication, authorization, or session-handling flaws in the sidecar
- Secret exposure (connection strings, API keys, LLM credentials)

## Responsible Disclosure

Responsible disclosure is appreciated. Reporters who follow this policy will
be credited in the release notes for the fix, unless they request to remain
anonymous.
