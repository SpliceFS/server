package share

import (
	"container/list"
	"sync"

	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/server/internal/digestcache"
)

// lru is a fixed-size least-recently-used cache of digest pairs, keyed like the entries of
// the digest store, so that both agree on when a file has changed. It is safe for concurrent
// use.
type lru struct {
	mu      sync.Mutex
	limit   int
	order   *list.List // front is the most recently used
	entries map[digestcache.Key]*list.Element
}

// lruEntry is the value stored in the list.
type lruEntry struct {
	key     digestcache.Key
	digests api.FileDigests
}

// newLRU returns a cache holding at most limit entries.
func newLRU(limit int) *lru {
	return &lru{limit: limit, order: list.New(), entries: make(map[digestcache.Key]*list.Element)}
}

// entryOf returns the cache entry held by element. The cache only ever stores *lruEntry
// values, so a false result means the list was changed from outside.
func entryOf(element *list.Element) (*lruEntry, bool) {
	entry, ok := element.Value.(*lruEntry)
	return entry, ok
}

// get returns the digests stored for key and marks them as recently used.
func (c *lru) get(key digestcache.Key) (api.FileDigests, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, found := c.entries[key]
	if !found {
		return api.FileDigests{}, false
	}
	entry, ok := entryOf(element)
	if !ok {
		return api.FileDigests{}, false
	}
	c.order.MoveToFront(element)
	return entry.digests, true
}

// put stores the digests for key, evicting the least recently used entry when the
// cache is full.
func (c *lru) put(key digestcache.Key, digests api.FileDigests) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, found := c.entries[key]; found {
		if entry, ok := entryOf(element); ok {
			entry.digests = digests
			c.order.MoveToFront(element)
			return
		}
	}
	c.entries[key] = c.order.PushFront(&lruEntry{key: key, digests: digests})
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
