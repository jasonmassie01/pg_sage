package notify

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Mutes in sage.notification_mutes (ux-09b): one row per database and
// class. A second mute of the same pair keeps the later until, so two
// admins muting at once leave one mute that ends when the longer one
// would. An ended row stays until it is replaced or removed: every read
// compares until with now, so no sweep is needed.

const muteColumns = `database, class, until, actor, reason, created_at`

// putMuteSQL inserts a mute or extends the stored one. The row is replaced
// (actor, reason, created_at too) only when the new until is later.
const putMuteSQL = `/* pg_sage */ INSERT INTO sage.notification_mutes AS m
    (database, class, until, actor, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (database, class) DO UPDATE SET
    until      = GREATEST(m.until, EXCLUDED.until),
    actor      = CASE WHEN EXCLUDED.until > m.until THEN EXCLUDED.actor ELSE m.actor END,
    reason     = CASE WHEN EXCLUDED.until > m.until THEN EXCLUDED.reason ELSE m.reason END,
    created_at = CASE WHEN EXCLUDED.until > m.until THEN EXCLUDED.created_at
                      ELSE m.created_at END
RETURNING ` + muteColumns

// PutMute stores m, or extends the stored mute of its database and class,
// and returns the mute now in force. The caller validates m
// (ValidateMute); the table's checks refuse what slips through.
func (s *PoolStore) PutMute(ctx context.Context, m Mute) (Mute, error) {
	if s.pool == nil {
		return Mute{}, fmt.Errorf("mute %q: notification pool is nil", m.Database)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := scanMute(s.pool.QueryRow(qctx, putMuteSQL, m.Database, m.Class, m.Until,
		m.Actor, m.Reason))
	if err != nil {
		return Mute{}, fmt.Errorf("mute %q class %q: %w", m.Database, m.Class, err)
	}
	return out, nil
}

// DeleteMute removes the mute of database and class (unmute) and reports
// whether there was one.
func (s *PoolStore) DeleteMute(ctx context.Context, database, class string) (bool, error) {
	if s.pool == nil {
		return false, fmt.Errorf("unmute %q: notification pool is nil", database)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ DELETE FROM sage.notification_mutes
		WHERE database = $1 AND class = $2`, database, class)
	if err != nil {
		return false, fmt.Errorf("unmute %q class %q: %w", database, class, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListMutes returns the mutes in force at now, by database and class; a
// zero now lists every stored row, ended ones too.
func (s *PoolStore) ListMutes(ctx context.Context, now time.Time) ([]Mute, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("list mutes: notification pool is nil")
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+muteColumns+`
		  FROM sage.notification_mutes
		 WHERE $1::timestamptz IS NULL OR until > $1
		 ORDER BY database, class LIMIT 1000`, nullTime(now))
	if err != nil {
		return nil, fmt.Errorf("list mutes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mute, error) {
		return scanMute(r)
	})
	if err != nil {
		return nil, fmt.Errorf("list mutes: %w", err)
	}
	return out, nil
}

// ActiveMute implements MuteStore.
func (s *PoolStore) ActiveMute(ctx context.Context, database string, c Class,
	now time.Time) (bool, error) {
	if s.pool == nil {
		return false, fmt.Errorf("read mutes of %q: notification pool is nil", database)
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var on bool
	err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT EXISTS (
		SELECT 1 FROM sage.notification_mutes
		 WHERE database = $1 AND class IN ('', $2) AND until > $3)`,
		database, string(c), now).Scan(&on)
	if err != nil {
		return false, fmt.Errorf("read mutes of %q: %w", database, err)
	}
	return on, nil
}

func scanMute(row pgx.Row) (Mute, error) {
	var m Mute
	err := row.Scan(&m.Database, &m.Class, &m.Until, &m.Actor, &m.Reason, &m.CreatedAt)
	return m, err
}

// muteKey is a mute's identity in the MemoryStore.
type muteKey struct{ database, class string }

// PutMute stores m like the pool store: the later until wins.
func (m *MemoryStore) PutMute(_ context.Context, mu Mute) (Mute, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mutes == nil {
		m.mutes = map[muteKey]Mute{}
	}
	k := muteKey{mu.Database, mu.Class}
	if cur, ok := m.mutes[k]; ok && !mu.Until.After(cur.Until) {
		return cur, nil
	}
	mu.CreatedAt = time.Now().UTC()
	m.mutes[k] = mu
	return mu, nil
}

// DeleteMute removes a mute like the pool store.
func (m *MemoryStore) DeleteMute(_ context.Context, database, class string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := muteKey{database, class}
	_, ok := m.mutes[k]
	delete(m.mutes, k)
	return ok, nil
}

// ListMutes lists mutes like the pool store.
func (m *MemoryStore) ListMutes(_ context.Context, now time.Time) ([]Mute, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mute
	for _, mu := range m.mutes {
		if now.IsZero() || mu.Until.After(now) {
			out = append(out, mu)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Class < out[j].Class
	})
	return out, nil
}

// ActiveMute implements MuteStore.
func (m *MemoryStore) ActiveMute(_ context.Context, database string, c Class,
	now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, class := range []string{"", string(c)} {
		if mu, ok := m.mutes[muteKey{database, class}]; ok && mu.Until.After(now) {
			return true, nil
		}
	}
	return false, nil
}
