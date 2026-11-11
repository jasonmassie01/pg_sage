package schema

// ddlEvidenceSigningKey (audit-10) holds the Ed25519 key the sidecar signs
// evidence packs with when audit.evidence.signing_key_env is unset and an
// encryption key is: one row (id = 1), the private key sealed under the
// encryption key (crypto.Keyring, "sage-enc:v1:..."), and its public key
// id. Sidecars create it with INSERT ... ON CONFLICT DO NOTHING and read it
// back, so every replica signs with the same key.
const ddlEvidenceSigningKey = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.evidence_signing_key (
    id         integer PRIMARY KEY CHECK (id = 1),
    key_id     text NOT NULL CHECK (key_id <> ''),
    sealed     text NOT NULL CHECK (sealed <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);
`
