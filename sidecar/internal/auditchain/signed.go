package auditchain

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Marker links (audit-03b) carry no row (row_id 0) and sit in the chain
// like a truncate: they are hashed, ordered and exported like any link.
const (
	// OpCheckpoint is a signed checkpoint: its state is a SignedCheckpoint
	// vouching for the chain up to an earlier link.
	OpCheckpoint = "C"
	// OpPrune records links removed below a signed checkpoint: its state
	// is a PruneMark.
	OpPrune = "P"
)

// KindSignedCheckpoint: a checkpoint link whose signature does not verify
// under the trusted key, whose signed link is gone or rewritten, or a
// pruned chain whose anchoring checkpoint cannot be verified.
const KindSignedCheckpoint = "signed_checkpoint"

// ErrCheckpointSignature means a signed checkpoint does not verify.
var ErrCheckpointSignature = errors.New("auditchain: checkpoint signature does not verify")

// AlgEd25519 is the only checkpoint signature algorithm.
const AlgEd25519 = "ed25519"

// TrustedKey is the public key checkpoint signatures are checked with,
// and its id (evidence.KeyID of the evidence-signing key).
type TrustedKey struct {
	ID     string
	Public ed25519.PublicKey
}

// CheckpointSigner signs checkpoints (the evidence-signing key, audit-10).
type CheckpointSigner struct {
	ID  string
	Key ed25519.PrivateKey
}

// Trusted is the signer's public half.
func (s CheckpointSigner) Trusted() TrustedKey {
	pub, _ := s.Key.Public().(ed25519.PublicKey)
	return TrustedKey{ID: s.ID, Public: pub}
}

// SignedCheckpoint states that chain verified clean up to link Seq, whose
// hash is Hash; Signature is the hex Ed25519 signature of Message().
type SignedCheckpoint struct {
	Chain     string `json:"chain"`
	Seq       int64  `json:"seq"`
	Hash      string `json:"hash"`
	Alg       string `json:"alg"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// Message is the signed text: a version tag, the chain, the position and
// the hash, '|'-joined (no field can hold a '|').
func (c SignedCheckpoint) Message() []byte {
	return []byte("pg_sage audit checkpoint v1|" + c.Chain + "|" +
		strconv.FormatInt(c.Seq, 10) + "|" + c.Hash)
}

func validHash(h string) bool {
	b, err := hex.DecodeString(h)
	return err == nil && len(b) == sha256.Size
}

// SignCheckpoint signs chain's link seq with hash.
func SignCheckpoint(s CheckpointSigner, chain string, seq int64, hash string) (
	SignedCheckpoint, error) {
	switch {
	case len(s.Key) != ed25519.PrivateKeySize:
		return SignedCheckpoint{}, errors.New("auditchain: sign checkpoint: no Ed25519 key")
	case s.ID == "":
		return SignedCheckpoint{}, errors.New("auditchain: sign checkpoint: the key has no id")
	case !chainName.MatchString(chain):
		return SignedCheckpoint{}, fmt.Errorf("auditchain: sign checkpoint: chain %q is "+
			"not a chain name", chain)
	case seq < 1:
		return SignedCheckpoint{}, fmt.Errorf("auditchain: sign checkpoint: position %d "+
			"is not a link", seq)
	case !validHash(hash):
		return SignedCheckpoint{}, fmt.Errorf("auditchain: sign checkpoint: %q is not a "+
			"sha256 hex hash", hash)
	}
	c := SignedCheckpoint{Chain: chain, Seq: seq, Hash: hash, Alg: AlgEd25519, KeyID: s.ID}
	c.Signature = hex.EncodeToString(ed25519.Sign(s.Key, c.Message()))
	return c, nil
}

// Verify checks c's signature under k. A checkpoint signed by another key
// id, or whose signature fails, is ErrCheckpointSignature.
func (c SignedCheckpoint) Verify(k TrustedKey) error {
	if len(k.Public) != ed25519.PublicKeySize {
		return errors.New("auditchain: verify checkpoint: no Ed25519 public key")
	}
	if c.Alg != AlgEd25519 {
		return fmt.Errorf("%w: algorithm %q, want %s", ErrCheckpointSignature, c.Alg,
			AlgEd25519)
	}
	if c.KeyID != k.ID {
		return fmt.Errorf("%w: signed by key %s, the trusted key is %s",
			ErrCheckpointSignature, c.KeyID, k.ID)
	}
	sig, err := hex.DecodeString(c.Signature)
	if err != nil || !ed25519.Verify(k.Public, c.Message(), sig) {
		return fmt.Errorf("%w: %s link %d under key %s", ErrCheckpointSignature, c.Chain,
			c.Seq, c.KeyID)
	}
	return nil
}

// Payload is the canonical JSON a checkpoint link stores as its state.
func (c SignedCheckpoint) Payload() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("auditchain: encode checkpoint: %w", err)
	}
	return string(b), nil
}

// ParseSignedCheckpoint reads a checkpoint link's state.
func ParseSignedCheckpoint(payload string) (SignedCheckpoint, error) {
	var c SignedCheckpoint
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return SignedCheckpoint{}, fmt.Errorf("auditchain: checkpoint payload: %w", err)
	}
	if c.Chain == "" || c.Seq < 1 || c.Hash == "" || c.Signature == "" {
		return SignedCheckpoint{}, errors.New("auditchain: checkpoint payload lacks the " +
			"chain, position, hash or signature")
	}
	return c, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// appendMarkerSQL appends a marker link through the chain's own append
// function (its lock, its hash), returning the position it took.
const appendMarkerSQL = `/* pg_sage audit_chain v1 */ SELECT o_seq
	FROM sage.audit_chain_append($1, 0, $2, 1, $3, $4)`

// linkOpAtSQL reads one link's hash and operation.
const linkOpAtSQL = `/* pg_sage audit_chain v1 */ SELECT hash, op
	FROM sage.audit_chain_link WHERE chain = $1 AND seq = $2`

// AppendSignedCheckpoint signs the head a clean full or incremental report
// reached and appends it to the chain as a checkpoint link, which the SIEM
// export then carries off-box. It appends nothing for an empty chain, or
// when the verified head is itself a checkpoint link (nothing new to
// vouch for), and returns the checkpoint link's position either way. The
// head link must still carry the hash the report verified.
func AppendSignedCheckpoint(ctx context.Context, db Querier, rep Report,
	s CheckpointSigner) (int64, bool, error) {
	if err := checkpointable(rep); err != nil {
		return 0, false, err
	}
	if db == nil {
		return 0, false, fmt.Errorf("auditchain: %s sign checkpoint: no database", rep.Chain)
	}
	if rep.HeadSeq == 0 {
		return 0, false, nil
	}
	var hash, op string
	err := db.QueryRow(ctx, linkOpAtSQL, rep.Chain, rep.HeadSeq).Scan(&hash, &op)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("auditchain: %s link %d: %w", rep.Chain, rep.HeadSeq, err)
	}
	if err != nil || hash != rep.HeadHash {
		return 0, false, fmt.Errorf("auditchain: %s: link %d changed after it was "+
			"verified; not signing it", rep.Chain, rep.HeadSeq)
	}
	if op == OpCheckpoint {
		return rep.HeadSeq, false, nil
	}
	cp, err := SignCheckpoint(s, rep.Chain, rep.HeadSeq, rep.HeadHash)
	if err != nil {
		return 0, false, err
	}
	seq, err := appendMarker(ctx, db, rep.Chain, OpCheckpoint, cp)
	return seq, err == nil, err
}

// appendMarker appends a marker link whose state is payload's JSON.
func appendMarker(ctx context.Context, db Querier, chain, op string, payload any) (int64,
	error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("auditchain: %s encode %s link: %w", chain, op, err)
	}
	state := string(b)
	var seq int64
	if err := db.QueryRow(ctx, appendMarkerSQL, chain, op, sha256Hex(state),
		state).Scan(&seq); err != nil {
		return 0, fmt.Errorf("auditchain: %s append %s link: %w", chain, op, err)
	}
	return seq, nil
}
