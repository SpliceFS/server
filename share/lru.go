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

// newDigestKey builds the cache key for the file at path relPath.
func newDigestKey(relPath string, info Info) digestKey {
	return digestKey{path: relPath, size: info.Size, mtime: info.MTime.UnixNano()}
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

// entryOf returns the cache entry held by element. The cache only ever stores *lruEntry
// values, so a false result means the list was changed from outside.
func entryOf(element *list.Element) (*lruEntry, bool) {
	entry, ok := element.Value.(*lruEntry)
	return entry, ok
}

// get returns the digest stored for key and marks it as recently used.
func (c *lru) get(key digestKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, found := c.entries[key]
	if !found {
		return "", false
	}
	entry, ok := entryOf(element)
	if !ok {
		return "", false
	}
	c.order.MoveToFront(element)
	return entry.digest, true
}

// put stores the digest for key, evicting the least recently used entry when the
// cache is full.
func (c *lru) put(key digestKey, digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, found := c.entries[key]; found {
		if entry, ok := entryOf(element); ok {
			entry.digest = digest
			c.order.MoveToFront(element)
			return
		}
	}
	c.entries[key] = c.order.PushFront(&lruEntry{key: key, digest: digest})
	if c.order.Len() > c.limit {
		c.evictOldest()
	}
}

// evictOldest drops the least recently used entry.
func (c *lru) evictOldest() {
	oldest := c.order.Back()
	c.order.Remove(oldest)
	if entry, ok := entryOf(oldest); ok {
		delete(c.entries, entry.key)
	}
}
