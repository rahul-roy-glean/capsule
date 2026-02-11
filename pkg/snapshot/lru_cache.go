package snapshot

import (
	"container/list"
	"sync"
)

// LRUCache is a thread-safe LRU cache for chunk data
type LRUCache struct {
	capacity int
	size     int64    // Current size in bytes
	maxSize  int64    // Maximum size in bytes
	items    map[string]*list.Element
	order    *list.List
	mu       sync.RWMutex
}

type cacheEntry struct {
	key  string
	data []byte
}

// NewLRUCache creates a new LRU cache with the given maximum size in bytes
func NewLRUCache(maxSizeBytes int64) *LRUCache {
	return &LRUCache{
		maxSize: maxSizeBytes,
		items:   make(map[string]*list.Element),
		order:   list.New(),
	}
}

// Get retrieves an item from the cache
func (c *LRUCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		// Move to front (most recently used)
		c.order.MoveToFront(elem)
		return elem.Value.(*cacheEntry).data, true
	}
	return nil, false
}

// Put adds an item to the cache
func (c *LRUCache) Put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	dataSize := int64(len(data))

	// If this single item is larger than max size, don't cache it
	if dataSize > c.maxSize {
		return
	}

	// If key exists, update it
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		c.size -= int64(len(entry.data))
		entry.data = data
		c.size += dataSize
		c.evictIfNeeded()
		return
	}

	// Evict items until we have room
	for c.size+dataSize > c.maxSize && c.order.Len() > 0 {
		c.evictOldest()
	}

	// Add new item
	entry := &cacheEntry{key: key, data: data}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	c.size += dataSize
	c.capacity = c.order.Len()
}

// evictOldest removes the least recently used item
func (c *LRUCache) evictOldest() {
	elem := c.order.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}

// evictIfNeeded evicts items until under max size
func (c *LRUCache) evictIfNeeded() {
	for c.size > c.maxSize && c.order.Len() > 0 {
		c.evictOldest()
	}
}

// removeElement removes an element from the cache
func (c *LRUCache) removeElement(elem *list.Element) {
	c.order.Remove(elem)
	entry := elem.Value.(*cacheEntry)
	delete(c.items, entry.key)
	c.size -= int64(len(entry.data))
	c.capacity = c.order.Len()
}

// Remove removes a specific item from the cache
func (c *LRUCache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.removeElement(elem)
	}
}

// Clear clears the cache
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.order.Init()
	c.size = 0
	c.capacity = 0
}

// Size returns the current size of the cache in bytes
func (c *LRUCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.size
}

// Len returns the number of items in the cache
func (c *LRUCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order.Len()
}

// Stats returns cache statistics
func (c *LRUCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CacheStats{
		Size:      c.size,
		MaxSize:   c.maxSize,
		ItemCount: c.order.Len(),
	}
}

// CacheStats holds cache statistics
type CacheStats struct {
	Size      int64
	MaxSize   int64
	ItemCount int
}
