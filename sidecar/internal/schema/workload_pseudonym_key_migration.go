package schema

// ddlWorkloadPseudonymKey (evid-02c) holds the 32-byte key that keys the
// workload pack model tier's identifier HMAC (HashIdentifiers): one row
// (id = 1), hex-encoded and sealed under the encryption key
// (crypto.Keyring, "sage-enc:v1:..."). Sidecars create it with INSERT ...
// ON CONFLICT DO NOTHING and read it back, so every replica hashes a name
// to the same pseudonym, across restarts and encryption-key rotations.
const ddlWorkloadPseudonymKey = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.workload_pseudonym_key (
    id         integer PRIMARY KEY CHECK (id = 1),
    sealed     text NOT NULL CHECK (sealed <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);
`
