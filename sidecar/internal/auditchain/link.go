package auditchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Genesis is the predecessor hash of a chain's first link.
var Genesis = strings.Repeat("0", 64)

// Link is one entry of a chain.
type Link struct {
	Seq        int64     `json:"seq"`
	RowID      int64     `json:"row_id"`
	Op         string    `json:"op"` // I, U, D or T
	V          int       `json:"v"`
	SealedHash string    `json:"sealed_hash"`
	State      string    `json:"state"`
	PrevHash   string    `json:"prev_hash"`
	Hash       string    `json:"hash"`
	At         time.Time `json:"at"`
	DBUser     string    `json:"db_user"`
	App        string    `json:"app"`
}

// LinkHash recomputes a link's hash: the sha256 hex of
// prev_hash|seq|op|row_id|v|sealed_hash|state. Every field but state has a
// fixed alphabet, so the preimage is unambiguous.
func LinkHash(l Link) string {
	pre := strings.Join([]string{l.PrevHash, strconv.FormatInt(l.Seq, 10), l.Op,
		strconv.FormatInt(l.RowID, 10), strconv.Itoa(l.V), l.SealedHash, l.State}, "|")
	sum := sha256.Sum256([]byte(pre))
	return hex.EncodeToString(sum[:])
}

// Querier is what the verifier reads through (a pool, connection or tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const linkColumns = `seq, row_id, op, v, sealed_hash, state, prev_hash, hash, at,
	db_user, app`

// HeadSQL reads a chain's last link.
const HeadSQL = `/* pg_sage audit_chain v1 */ SELECT seq, hash FROM sage.audit_chain_link
	WHERE chain = $1 ORDER BY seq DESC LIMIT 1`

// RowLinksSQL reads every link of one row, in order.
const RowLinksSQL = `/* pg_sage audit_chain v1 */ SELECT ` + linkColumns + `
	FROM sage.audit_chain_link WHERE chain = $1 AND row_id = $2 ORDER BY seq`

const rangeLinksSQL = `/* pg_sage audit_chain v1 */ SELECT ` + linkColumns + `
	FROM sage.audit_chain_link WHERE chain = $1 AND seq >= $2 AND seq <= $3
	ORDER BY seq LIMIT $4`

const linkAtSQL = `/* pg_sage audit_chain v1 */ SELECT hash FROM sage.audit_chain_link
	WHERE chain = $1 AND seq = $2`

// Head returns a chain's last position and hash (0 and "" when empty).
func Head(ctx context.Context, q Querier, chain string) (int64, string, error) {
	var seq int64
	var hash string
	err := q.QueryRow(ctx, HeadSQL, chain).Scan(&seq, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("auditchain: %s head: %w", chain, err)
	}
	return seq, hash, nil
}

// LinksForRow returns every link of one row of chain, in order.
func LinksForRow(ctx context.Context, q Querier, chain string, rowID int64) ([]Link, error) {
	rows, err := q.Query(ctx, RowLinksSQL, chain, rowID)
	if err != nil {
		return nil, fmt.Errorf("auditchain: %s links of row %d: %w", chain, rowID, err)
	}
	links, err := scanLinks(rows)
	if err != nil {
		return nil, fmt.Errorf("auditchain: %s links of row %d: %w", chain, rowID, err)
	}
	return links, nil
}

// LinksInRange returns up to limit links of chain from seq from to to.
func LinksInRange(ctx context.Context, q Querier, chain string, from, to int64,
	limit int) ([]Link, error) {
	rows, err := q.Query(ctx, rangeLinksSQL, chain, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("auditchain: %s links %d-%d: %w", chain, from, to, err)
	}
	links, err := scanLinks(rows)
	if err != nil {
		return nil, fmt.Errorf("auditchain: %s links %d-%d: %w", chain, from, to, err)
	}
	return links, nil
}

func scanLinks(rows pgx.Rows) ([]Link, error) {
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Seq, &l.RowID, &l.Op, &l.V, &l.SealedHash, &l.State,
			&l.PrevHash, &l.Hash, &l.At, &l.DBUser, &l.App); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
