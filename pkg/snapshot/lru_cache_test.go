package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// keysInSameShard returns n keys that hash to the same shard.
func keysInSameShard(n int) []string {
	buckets := make(map[uint32][]string)
	c := &LRUCache{} // just need the shard function
	for i := 0; len(buckets) == 0 || len(buckets[targetShard(buckets)]) < n; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("key-%d", i)))
		key := hex.EncodeToString(h[:8])
		s := c.shard(key)
		// find shard index
		for idx := range c.shards {
			// shards are nil here, compare pointer identity won't work
			_ = idx
		}
		// Use the hash function directly
		var hv uint32
		for j := 0; j < len(key); j++ {
			hv = hv*31 + uint32(key[j])
		}
		idx := hv % numShards
		buckets[idx] = append(buckets[idx], key)
		_ = s
	}
	shard := targetShard(buckets)
	return buckets[shard][:n]
}

func targetShard(buckets map[uint32][]string) uint32 {
	var best uint32
	var bestLen int
	for k, v := range buckets {
		if len(v) > bestLen {
			best = k
			bestLen = len(v)
		}
	}
	return best
}

func TestLRUCache_Basic(t *testing.T) {
	cache := NewLRUCache(1600) // 100 bytes per shard

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
	// Use a cache where each shard holds at most 2 items of 8 bytes
	cache := NewLRUCache(numShards * 20) // 20 bytes per shard

	// Get 3 keys that hash to the same shard
	keys := keysInSameShard(3)

	cache.Put(keys[0], []byte("12345678")) // 8 bytes
	cache.Put(keys[1], []byte("12345678")) // 8 bytes
	cache.Put(keys[2], []byte("12345678")) // 8 bytes - should evict keys[0]

	// keys[0] should be evicted
	_, ok := cache.Get(keys[0])
	if ok {
		t.Error("keys[0] should have been evicted")
	}

	// keys[1] and keys[2] should still exist
	_, ok = cache.Get(keys[1])
	if !ok {
		t.Error("keys[1] should exist")
	}
	_, ok = cache.Get(keys[2])
	if !ok {
		t.Error("keys[2] should exist")
	}
}

func TestLRUCache_LRUOrder(t *testing.T) {
	cache := NewLRUCache(numShards * 20) // 20 bytes per shard

	keys := keysInSameShard(3)

	cache.Put(keys[0], []byte("12345678")) // 8 bytes
	cache.Put(keys[1], []byte("12345678")) // 8 bytes

	// Access keys[0] to make it recently used
	cache.Get(keys[0])

	// Add keys[2] - should evict keys[1] (least recently used), not keys[0]
	cache.Put(keys[2], []byte("12345678")) // 8 bytes

	// keys[1] should be evicted
	_, ok := cache.Get(keys[1])
	if ok {
		t.Error("keys[1] should have been evicted (LRU)")
	}

	// keys[0] should still exist (was accessed)
	_, ok = cache.Get(keys[0])
	if !ok {
		t.Error("keys[0] should still exist (recently accessed)")
	}
}

func TestLRUCache_Update(t *testing.T) {
	cache := NewLRUCache(1600)

	cache.Put("key1", []byte("original"))
	cache.Put("key1", []byte("updated"))

	data, ok := cache.Get("key1")
	if !ok {
		t.Fatal("Expected to find key1")
	}
	if string(data) != "updated" {
		t.Errorf("Expected 'updated', got '%s'", string(data))
	}

	if cache.Len() != 1 {
		t.Errorf("Expected 1 item, got %d", cache.Len())
	}
}

func TestLRUCache_OversizedItem(t *testing.T) {
	cache := NewLRUCache(numShards * 10) // 10 bytes per shard

	// Try to add item larger than any shard
	cache.Put("big", []byte("this is way too large for the cache"))

	_, ok := cache.Get("big")
	if ok {
		t.Error("Oversized item should not be cached")
	}
}

func TestLRUCache_Remove(t *testing.T) {
	cache := NewLRUCache(1600)

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
	cache := NewLRUCache(1600)

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
	cache := NewLRUCache(16000)

	cache.Put("key1", []byte("12345")) // 5 bytes
	cache.Put("key2", []byte("123"))   // 3 bytes

	stats := cache.Stats()

	if stats.Size != 8 {
		t.Errorf("Expected size 8, got %d", stats.Size)
	}

	if stats.MaxSize != 16000 {
		t.Errorf("Expected maxSize 16000, got %d", stats.MaxSize)
	}

	if stats.ItemCount != 2 {
		t.Errorf("Expected 2 items, got %d", stats.ItemCount)
	}
}
