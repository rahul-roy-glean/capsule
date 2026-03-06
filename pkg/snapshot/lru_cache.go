package snapshot

import (
	"container/list"
	"sync"
	"sync/atomic"
)

const numShards = 64

// LRUCache is a thread-safe, sharded LRU cache for chunk data.
// It distributes keys across multiple shards to reduce lock contention
// when many goroutines (UFFD handlers, FUSE disks, eager fetchers) access
// the cache concurrently.
type LRUCache struct {
	shards     [numShards]*lruShard
	maxSize    int64
	hits       atomic.Int64
	misses     atomic.Int64
	evictions  atomic.Int64
}

// lruShard is a single shard of the LRU cache with its own lock.
type lruShard struct {
	size    int64 // Current size in bytes
	maxSize int64 // Maximum size in bytes for this shard
	items   map[string]*list.Element
	order   *list.List
	mu      sync.Mutex
	parent  *LRUCache
}

type cacheEntry struct {
	key  string
	data []byte
}

// NewLRUCache creates a new sharded LRU cache with the given maximum size in bytes.
// The total capacity is distributed evenly across shards. Individual shards may
// temporarily exceed their quota slightly, but the aggregate stays bounded.
func NewLRUCache(maxSizeBytes int64) *LRUCache {
	c := &LRUCache{maxSize: maxSizeBytes}
	// Each shard gets an equal share, with remainder going to the last shard.
	perShard := maxSizeBytes / numShards
	remainder := maxSizeBytes % numShards
	for i := range c.shards {
		shardMax := perShard
		if i == numShards-1 {
			shardMax += remainder
		}
		c.shards[i] = &lruShard{
			maxSize: shardMax,
			items:   make(map[string]*list.Element),
			order:   list.New(),
			parent:  c,
		}
	}
	return c
}

// shard returns the shard for a given key using FNV-inspired hash.
func (c *LRUCache) shard(key string) *lruShard {
	var h uint32
	for i := 0; i < len(key); i++ {
		h = h*31 + uint32(key[i])
	}
	return c.shards[h%numShards]
}

// Get retrieves an item from the cache
func (c *LRUCache) Get(key string) ([]byte, bool) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if elem, ok := s.items[key]; ok {
		s.order.MoveToFront(elem)
		c.hits.Add(1)
		return elem.Value.(*cacheEntry).data, true
	}
	c.misses.Add(1)
	return nil, false
}

// Put adds an item to the cache
func (c *LRUCache) Put(key string, data []byte) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	dataSize := int64(len(data))

	if dataSize > s.maxSize {
		return
	}

	// If key exists, update it
	if elem, ok := s.items[key]; ok {
		s.order.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		s.size -= int64(len(entry.data))
		entry.data = data
		s.size += dataSize
		s.evictIfNeeded()
		return
	}

	// Evict items until we have room
	for s.size+dataSize > s.maxSize && s.order.Len() > 0 {
		s.evictOldest()
	}

	// Add new item
	entry := &cacheEntry{key: key, data: data}
	elem := s.order.PushFront(entry)
	s.items[key] = elem
	s.size += dataSize
}

// evictOldest removes the least recently used item
func (s *lruShard) evictOldest() {
	elem := s.order.Back()
	if elem != nil {
		s.removeElement(elem)
		if s.parent != nil {
			s.parent.evictions.Add(1)
		}
	}
}

// evictIfNeeded evicts items until under max size
func (s *lruShard) evictIfNeeded() {
	for s.size > s.maxSize && s.order.Len() > 0 {
		s.evictOldest()
	}
}

// removeElement removes an element from the shard
func (s *lruShard) removeElement(elem *list.Element) {
	s.order.Remove(elem)
	entry := elem.Value.(*cacheEntry)
	delete(s.items, entry.key)
	s.size -= int64(len(entry.data))
}

// Remove removes a specific item from the cache
func (c *LRUCache) Remove(key string) {
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if elem, ok := s.items[key]; ok {
		s.removeElement(elem)
	}
}

// Clear clears the entire cache
func (c *LRUCache) Clear() {
	for _, s := range c.shards {
		s.mu.Lock()
		s.items = make(map[string]*list.Element)
		s.order.Init()
		s.size = 0
		s.mu.Unlock()
	}
}

// Size returns the current size of the cache in bytes
func (c *LRUCache) Size() int64 {
	var total int64
	for _, s := range c.shards {
		s.mu.Lock()
		total += s.size
		s.mu.Unlock()
	}
	return total
}

// Len returns the number of items in the cache
func (c *LRUCache) Len() int {
	var total int
	for _, s := range c.shards {
		s.mu.Lock()
		total += s.order.Len()
		s.mu.Unlock()
	}
	return total
}

// Stats returns cache statistics
func (c *LRUCache) Stats() CacheStats {
	return CacheStats{
		Size:      c.Size(),
		MaxSize:   c.maxSize,
		ItemCount: c.Len(),
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}

// CacheStats holds cache statistics
type CacheStats struct {
	Size      int64
	MaxSize   int64
	ItemCount int
	Hits      int64
	Misses    int64
	Evictions int64
}
