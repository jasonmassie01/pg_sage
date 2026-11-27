package publish

import (
	"bytes"
	"fmt"
	"html/template"
)

// page is index.html: a static page with no script and no external asset,
// so it can be hosted anywhere and read offline. html/template escapes
// every value, though every value is a number or a fixed code.
var page = template.Must(template.New("outcomes").Funcs(template.FuncMap{
	"pct": func(v float64) string { return fmt.Sprintf("%.2f%%", v*100) },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>pg_sage outcomes</title>
<style>
body{font-family:system-ui,sans-serif;max-width:52rem;margin:2rem auto;padding:0 1rem;
color:#1b1f24;background:#fff;line-height:1.5}
table{border-collapse:collapse;margin:0.5rem 0 1.5rem}
th,td{border:1px solid #c8ccd1;padding:0.25rem 0.6rem;text-align:right}
th:first-child,td:first-child{text-align:left}
.note{color:#57606a}
@media (prefers-color-scheme: dark){body{color:#e6edf3;background:#0d1117}
th,td{border-color:#30363d}.note{color:#8b949e}}
</style>
</head>
<body>
<h1>pg_sage outcomes: verified and reversible</h1>
{{if .B.Sample}}<p><strong>Sample data.</strong> This bundle is built from a fixture
ledger and signed with a published sample key: it shows the format, not a result.</p>
{{end}}<p>{{.B.Since}} window, {{.B.From.Format "2006-01-02"}} to
{{.B.To.Format "2006-01-02"}}, over {{.B.Databases}} database(s). Rubric
<code>{{.B.RubricVersion}}</code>, format <code>{{.B.Format}}</code>.</p>
{{if .B.Totals}}<p>{{.B.Statement}}.</p>
<h2>All changes</h2>
<table><tr><th>Changes</th><th>Improved</th><th>No change</th><th>Regressed</th>
<th>Inconclusive</th><th>Not judged yet</th><th>Reverted</th><th>Rollback failed</th></tr>
<tr>{{template "cells" .B.Totals}}</tr></table>
<h2>By action class</h2>{{template "buckets" .B.ByClass}}
<h2>By origin</h2>{{template "buckets" .B.ByOrigin}}
<h2>Reverted changes, by the verifier's verdict</h2>
<table><tr><th>Verdict</th><th>Reverted</th></tr>
{{range $v, $n := .B.RevertedByVerdict}}<tr><td>{{$v}}</td><td>{{$n}}</td></tr>
{{end}}</table>
{{else}}<p><strong>No changes in the window.</strong> pg_sage changed nothing on these
databases in this window, so there is nothing to count.</p>
{{end}}<h2>Verifier false positives</h2>
{{with .B.Verifier}}<p>Placebo back-test: {{.FalsePositives}} false positives in
{{.UsablePlacebos}} usable placebos over {{.Backtests}} back-test(s): {{pct .FPRate}}
(95% upper bound {{pct .FPWilsonUpper}}).</p>
{{else}}<p>Not measured: {{.B.VerifierStatement}}.</p>
{{end}}<p class="note">Classes and origins with fewer than {{.B.KFloor}} changes are
counted under "other". The bundle holds counts and fixed codes only: no database
names, hosts, SQL, identifiers or free text.</p>
<p class="note">Check it: <code>pg_sage_sidecar evidence verify outcomes.tar.gz
--public-key key.pem</code>{{if .KeyID}}, with the public key of signing key
<code>{{.KeyID}}</code>{{end}}. <code>sha256sum -c SHA256SUMS</code> checks the
loose files.</p>
</body>
</html>
{{define "cells"}}<td>{{.Changes}}</td><td>{{.Improved}}</td><td>{{.NoChange}}</td>
<td>{{.Regressed}}</td><td>{{.Inconclusive}}</td><td>{{.Pending}}</td><td>{{.Reverted}}</td>
<td>{{.RollbackFailed}}</td>{{end}}
{{define "buckets"}}<table><tr><th></th><th>Changes</th><th>Improved</th><th>No change</th>
<th>Regressed</th><th>Inconclusive</th><th>Not judged yet</th><th>Reverted</th>
<th>Rollback failed</th></tr>
{{range .}}<tr><td>{{.Key}}</td>{{template "cells" .Counts}}</tr>
{{end}}</table>{{end}}
`))

// RenderHTML is index.html for b; keyID names the signing key ("" none).
func RenderHTML(b Bundle, keyID string) []byte {
	var buf bytes.Buffer
	if err := page.Execute(&buf, struct {
		B     Bundle
		KeyID string
	}{b, keyID}); err != nil {
		// The template is fixed and every field it reads exists, so an
		// error here is a programming error caught by the tests.
		panic(fmt.Sprintf("publish: render index.html: %v", err))
	}
	return buf.Bytes()
}
