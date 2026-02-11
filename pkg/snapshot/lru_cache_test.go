package snapshot

import (
	"testing"
)

func TestLRUCache_Basic(t *testing.T) {
	cache := NewLRUCache(100) // 100 bytes max

	// Test put and get
	cache.Put("key1", []byte("hello"))
	data, ok := cache.Get("key1")
	if !ok {
		t.Fatal("Expected to find key1")
	}
	if string(data) != "hello" {
		t.Errorf("Expected 'hello', got '%s'", string(data))
	}

	// Test miss
	_, ok = cache.Get("nonexistent")
	if ok {
		t.Error("Expected miss for nonexistent key")
	}
}

func TestLRUCache_Eviction(t *testing.T) {
	cache := NewLRUCache(20) // 20 bytes max

	// Add items that exceed capacity
	cache.Put("key1", []byte("12345678")) // 8 bytes
	cache.Put("key2", []byte("12345678")) // 8 bytes
	cache.Put("key3", []byte("12345678")) // 8 bytes - should evict key1

	// key1 should be evicted
	_, ok := cache.Get("key1")
	if ok {
		t.Error("key1 should have been evicted")
	}

	// key2 and key3 should still exist
	_, ok = cache.Get("key2")
	if !ok {
		t.Error("key2 should exist")
	}
	_, ok = cache.Get("key3")
	if !ok {
		t.Error("key3 should exist")
	}
}

func TestLRUCache_LRUOrder(t *testing.T) {
	cache := NewLRUCache(20) // 20 bytes max

	// Add items
	cache.Put("key1", []byte("12345678")) // 8 bytes
	cache.Put("key2", []byte("12345678")) // 8 bytes

	// Access key1 to make it recently used
	cache.Get("key1")

	// Add key3 - should evict key2 (least recently used), not key1
	cache.Put("key3", []byte("12345678")) // 8 bytes

	// key2 should be evicted
	_, ok := cache.Get("key2")
	if ok {
		t.Error("key2 should have been evicted (LRU)")
	}

	// key1 should still exist (was accessed)
	_, ok = cache.Get("key1")
	if !ok {
		t.Error("key1 should still exist (recently accessed)")
	}
}

func TestLRUCache_Update(t *testing.T) {
	cache := NewLRUCache(100)

	cache.Put("key1", []byte("original"))
	cache.Put("key1", []byte("updated"))

	data, ok := cache.Get("key1")
	if !ok {
		t.Fatal("Expected to find key1")
	}
	if string(data) != "updated" {
		t.Errorf("Expected 'updated', got '%s'", string(data))
	}

	// Size should reflect updated value, not both
	if cache.Len() != 1 {
		t.Errorf("Expected 1 item, got %d", cache.Len())
	}
}

func TestLRUCache_OversizedItem(t *testing.T) {
	cache := NewLRUCache(10) // 10 bytes max

	// Try to add item larger than cache
	cache.Put("big", []byte("this is way too large for the cache"))

	// Should not be cached
	_, ok := cache.Get("big")
	if ok {
		t.Error("Oversized item should not be cached")
	}
}

func TestLRUCache_Remove(t *testing.T) {
	cache := NewLRUCache(100)

	cache.Put("key1", []byte("data1"))
	cache.Put("key2", []byte("data2"))

	cache.Remove("key1")

	_, ok := cache.Get("key1")
	if ok {
		t.Error("key1 should have been removed")
	}

	_, ok = cache.Get("key2")
	if !ok {
		t.Error("key2 should still exist")
	}
}

func TestLRUCache_Clear(t *testing.T) {
	cache := NewLRUCache(100)

	cache.Put("key1", []byte("data1"))
	cache.Put("key2", []byte("data2"))
	cache.Put("key3", []byte("data3"))

	cache.Clear()

	if cache.Len() != 0 {
		t.Errorf("Expected 0 items after clear, got %d", cache.Len())
	}

	if cache.Size() != 0 {
		t.Errorf("Expected 0 bytes after clear, got %d", cache.Size())
	}
}

func TestLRUCache_Stats(t *testing.T) {
	cache := NewLRUCache(1000)

	cache.Put("key1", []byte("12345")) // 5 bytes
	cache.Put("key2", []byte("123"))   // 3 bytes

	stats := cache.Stats()

	if stats.Size != 8 {
		t.Errorf("Expected size 8, got %d", stats.Size)
	}

	if stats.MaxSize != 1000 {
		t.Errorf("Expected maxSize 1000, got %d", stats.MaxSize)
	}

	if stats.ItemCount != 2 {
		t.Errorf("Expected 2 items, got %d", stats.ItemCount)
	}
}
