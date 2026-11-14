// Package docscontract holds static checks on the MkDocs site built from
// docs/ and mkdocs.yml: every page is reachable or deliberately excluded,
// no internal note is published, and every relative link resolves. It also
// checks that VERSION, CHANGELOG.md and SECURITY.md name the same release,
// that no guide asks for an older Go than sidecar/go.mod (ci-10), and that
// every supported-PostgreSQL claim matches the majors CI tests
// (footprint-10b). The checks live in the package's tests; this file only
// names the package.
package docscontract
