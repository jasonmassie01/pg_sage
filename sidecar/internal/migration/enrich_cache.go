package migration

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// The reply cache keeps one model answer per statement identity for a
// day, at most enrichCacheSize of them, least recently used out first.
const (
	enrichCacheSize = 256
	enrichCacheTTL  = 24 * time.Hour
)

// enrichKey is the identity a model answer is cached under: the rule, the
// sanitized and normalized statement, and the table's schema text, so a
// schema change asks again (migration-10).
func enrichKey(ruleID, statement, schema string) string {
	h := sha256.New()
	for _, part := range []string{ruleID, normalizeMigrationSQL(sanitizeDDL(statement)), schema} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// replyCache is an LRU of model replies with a time to live. A nil cache
// stores nothing. Safe for concurrent use.
type replyCache struct {
	mu    sync.Mutex
	size  int
	ttl   time.Duration
	now   func() time.Time
	order *list.List // front: most recently used *cacheEntry
	items map[string]*list.Element
}

type cacheEntry struct {
	key   string
	reply string
	at    time.Time
}

func newReplyCache(size int, ttl time.Duration) *replyCache {
	return &replyCache{size: size, ttl: ttl, now: time.Now,
		order: list.New(), items: make(map[string]*list.Element)}
}

// get returns the reply cached for key while it is younger than the TTL.
func (c *replyCache) get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*cacheEntry)
	if c.now().Sub(e.at) >= c.ttl {
		c.order.Remove(el)
		delete(c.items, key)
		return "", false
	}
	c.order.MoveToFront(el)
	return e.reply, true
}

// put stores reply for key, evicting the least recently used entry when
// the cache is full.
func (c *replyCache) put(key, reply string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = &cacheEntry{key: key, reply: reply, at: c.now()}
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&cacheEntry{key: key, reply: reply, at: c.now()})
	for c.order.Len() > c.size {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}
