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

// FileDigests holds the head digest and the tail digest of one version of a file: the
// hex SHA-256 of its first wire.HeadDigestBytes bytes and of its last
// wire.TailDigestBytes bytes. They are cached together because one read of the file
// produces both.
type FileDigests struct {
	Head string
	Tail string
}

// lru is a fixed-size least-recently-used cache of digest pairs. It is safe for
// concurrent use.
type lru struct {
	mu      sync.Mutex
	limit   int
	order   *list.List // front is the most recently used
	entries map[digestKey]*list.Element
}

// lruEntry is the value stored in the list.
type lruEntry struct {
	key     digestKey
	digests FileDigests
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

// get returns the digests stored for key and marks them as recently used.
func (c *lru) get(key digestKey) (FileDigests, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, found := c.entries[key]
	if !found {
		return FileDigests{}, false
	}
	entry, ok := entryOf(element)
	if !ok {
		return FileDigests{}, false
	}
	c.order.MoveToFront(element)
	return entry.digests, true
}

// put stores the digests for key, evicting the least recently used entry when the
// cache is full.
func (c *lru) put(key digestKey, digests FileDigests) {
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
