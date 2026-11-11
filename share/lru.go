package share

import (
	"container/list"
	"sync"
)

// digestKey identifies one version of a file: a change of size or modification time
// is a different key.
type digestKey struct {
	path  string
	size  int64
	mtime int64
}

// newDigestKey builds the cache key for the file at path p.
func newDigestKey(p string, info Info) digestKey {
	return digestKey{path: p, size: info.Size, mtime: info.MTime.UnixNano()}
}

// lru is a fixed-size least-recently-used cache of head digests. It is safe for
// concurrent use.
type lru struct {
	mu      sync.Mutex
	limit   int
	order   *list.List // front is the most recently used
	entries map[digestKey]*list.Element
}

// lruEntry is the value stored in the list.
type lruEntry struct {
	key    digestKey
	digest string
}

// newLRU returns a cache holding at most limit entries.
func newLRU(limit int) *lru {
	return &lru{limit: limit, order: list.New(), entries: make(map[digestKey]*list.Element)}
}

// get returns the digest stored for key and marks it as recently used.
func (c *lru) get(key digestKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).digest, true
}

// put stores the digest for key, evicting the least recently used entry when the
// cache is full.
func (c *lru) put(key digestKey, digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*lruEntry).digest = digest
		c.order.MoveToFront(el)
		return
	}
	c.entries[key] = c.order.PushFront(&lruEntry{key: key, digest: digest})
	if c.order.Len() > c.limit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*lruEntry).key)
	}
}
