# Security Policy

## Supported Versions

Only the latest minor release line receives security fixes. Earlier versions
are end-of-life and will not receive patches.

| Version | Supported          |
| ------- | ------------------ |
| 0.8.x   | Yes                |
| 0.7.x   | No (EOL)           |
| < 0.7   | No (EOL)           |

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

pg_sage is designed to run as a PostgreSQL superuser so that it can execute
DDL, manage extensions, and perform maintenance operations autonomously. As a
result, the following classes of vulnerability are considered HIGH severity by
default:

- Flaws in the executor that could allow unintended SQL to run
- Bypasses of SQL validation, allow-list, or trust-level gating
- Authentication, authorization, or session-handling flaws in the sidecar
- Secret exposure (connection strings, API keys, LLM credentials)

## Responsible Disclosure

Responsible disclosure is appreciated. Reporters who follow this policy will
be credited in the release notes for the fix, unless they request to remain
anonymous.
