# Publishing outcomes: verified and reversible

pg_sage changes a database only through its executor, which verifies every change against a
prediction and keeps a rollback. `pg_sage publish outcomes` turns that record into numbers you
can put on a public page: how many changes were applied, how many were verified improved, how
many made no difference or could not be judged, how many were reverted and why, and how often
the verifier calls an effect that did not happen. The numbers come from the
[outcome ledger](api.md) and the [verifier back-test](verification.md); the bundle is signed so
a reader can check it was not edited.

The [rubric](rubric.md) is published beside it: the eight dimensions Sage Review grades a
database on, each check, what reaches each grade and what reports `unknown`. A
[sample bundle](outcomes-sample/index.html) shows the format.

## The command

```bash
export SAGE_EVIDENCE_SIGNING_KEY_FILE=/run/secrets/evidence-key.pem
pg_sage publish outcomes --since 90d --out ./outcomes-2026q4 \
  --dsn "$APP_ONE_DSN" --dsn "$APP_TWO_DSN"
```

| Flag | Default | Meaning |
|---|---|---|
| `--out DIR` | required | Where the bundle goes. It must not exist, or be an empty directory: a bundle is never written over another. |
| `--since` | `90d` | The window: whole days from `1d` to `400d` (the ledger's retention), or a duration such as `36h`. |
| `--dsn DSN` | `SAGE_HISTORY_MONITORED_DSN`, else `SAGE_META_DB` | A database whose pg_sage tables are read. Repeat it for several databases; the bundle adds them up. |
| `--signing-key-env NAME` | `SAGE_EVIDENCE_SIGNING_KEY` | The variable (or its `_FILE` twin) holding the Ed25519 PKCS#8 PEM key that signs the bundle. |
| `--sample` | off | Write the sample bundle; reads no database and no key. |

Exit codes: `0` written; `1` a read or write failed, and nothing was written; `2` a usage error
(an unparseable or zero `--since`, an `--out` that is a file or a directory that is not empty,
no database, or no usable key).

A public bundle is always signed. Use the key `audit.evidence.signing_key_env` names, so the
bundle and the install's evidence packs share one public key
(`GET /api/v1/audit/evidence/key`). The key pg_sage generates and seals in the database never
leaves it, so it cannot sign a bundle.

The command only reads. Each database is read in one read-only, repeatable-read transaction
with a 60 s `statement_timeout` and a 5 s `lock_timeout`, so the ledger and the
back-test come from the same snapshot.
A window with more than 10,000 changes on one database exits 1: shorten `--since` so every
change is counted.

## What the bundle holds

| File | What it is |
|---|---|
| `outcomes.json` | The numbers (format `pg_sage-outcomes/1`). |
| `index.html` | The same numbers as a static page: no script, no external asset. |
| `outcomes.tar.gz` | The sealed evidence pack of the two files above, with its manifest and signature. |
| `manifest.json`, `manifest.sig`, `SHA256SUMS` | The pack's members, written loose so a static host serves the page and `sha256sum -c SHA256SUMS` checks the files. |

`outcomes.json` counts, for all changes, by action class and by origin (`plan`, `approved`,
`agent`, `autonomous`): changes, `improved`, `no_change`, `regressed`, `inconclusive` (judged
without a measurement: inconclusive, unverifiable, expired), `pending` (not judged yet),
`reverted` and `rollback_failed`. `reverted_by_verdict` says why changes were reverted, as the
verifier's verdict on each. `verifier` is the placebo false-positive rate over the newest
back-test of every database, with its Wilson 95% upper bound; without a back-test it is null
and `verifier_statement` says why.

An empty window says `no changes in the window`, and its counts are null rather than zeros.

## What never leaves the database

The bundle holds numbers and fixed codes only. Database names, hosts, SQL, object names, ids,
evidence ids, justifications and every other free-text field are left out by construction:

- An action type is named only when this build knows it (the earned action classes' types and
  the ledger's labelled ones); any other value is counted as `other`.
- An origin or verdict outside the ledger's vocabulary becomes `other` or `unknown`.
- A class or origin with fewer than 5 changes (`k_floor`) is counted under `other`, so no single
  database's action can be singled out.

A test on PostgreSQL 14 and 17 seeds a ledger with secret-like literals, the database's name
and a most-common value in its SQL, justification and verdict reason, and byte-scans every
bundle file, and every member of the pack, for them.

## Checking a bundle

```bash
pg_sage_sidecar evidence verify outcomes.tar.gz --public-key key.pem
```

The command checks every file against `SHA256SUMS` and the manifest, and the manifest's
signature by the key you trust. The sample bundle is signed with a sample key whose private
half is derived from a published seed: it verifies, and proves only the format.
