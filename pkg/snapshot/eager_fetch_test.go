package snapshot

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/util/boundedstack"
)

// TestEagerFetchQueue tests the eager fetch queue behavior
func TestEagerFetchQueue(t *testing.T) {
	stack, err := boundedstack.New[string](10)
	if err != nil {
		t.Fatalf("Failed to create stack: %v", err)
	}

	// Push some hashes
	hashes := []string{"hash1", "hash2", "hash3"}
	for _, h := range hashes {
		stack.Push(h)
	}

	// Should receive in LIFO order
	h, ok := stack.TryRecv()
	if !ok {
		t.Fatal("Expected to receive item")
	}
	if h != "hash3" {
		t.Errorf("Expected hash3, got %s", h)
	}

	h, ok = stack.TryRecv()
	if !ok {
		t.Fatal("Expected to receive item")
	}
	if h != "hash2" {
		t.Errorf("Expected hash2, got %s", h)
	}

	stack.Close()
}

// TestEagerFetchEviction tests that old items are evicted when queue is full
func TestEagerFetchEviction(t *testing.T) {
	stack, err := boundedstack.New[string](3)
	if err != nil {
		t.Fatalf("Failed to create stack: %v", err)
	}

	// Push more than capacity
	stack.Push("hash1")
	stack.Push("hash2")
	stack.Push("hash3")
	stack.Push("hash4") // Evicts hash1
	stack.Push("hash5") // Evicts hash2

	// Should have hash5, hash4, hash3 in LIFO order
	received := make([]string, 0)
	for {
		h, ok := stack.TryRecv()
		if !ok {
			break
		}
		received = append(received, h)
	}

	if len(received) != 3 {
		t.Errorf("Expected 3 items, got %d", len(received))
	}

	// Check order (LIFO)
	expected := []string{"hash5", "hash4", "hash3"}
	for i, h := range received {
		if h != expected[i] {
			t.Errorf("Position %d: expected %s, got %s", i, expected[i], h)
		}
	}
}

// MockChunkFetcher simulates chunk fetching for testing
type MockChunkFetcher struct {
	fetchCount    int64
	fetchedHashes []string
}

func (m *MockChunkFetcher) Fetch(hash string) error {
	atomic.AddInt64(&m.fetchCount, 1)
	m.fetchedHashes = append(m.fetchedHashes, hash)
	// Simulate some latency
	time.Sleep(time.Millisecond)
	return nil
}

func (m *MockChunkFetcher) GetFetchCount() int64 {
	return atomic.LoadInt64(&m.fetchCount)
}

// TestEagerFetchConcurrency tests concurrent access to the eager fetch queue
func TestEagerFetchConcurrency(t *testing.T) {
	stack, err := boundedstack.New[string](100)
	if err != nil {
		t.Fatalf("Failed to create stack: %v", err)
	}

	// Start multiple producers
	done := make(chan bool)
	for i := 0; i < 5; i++ {
		go func(id int) {
			for j := 0; j < 20; j++ {
				stack.Push("hash")
			}
			done <- true
		}(i)
	}

	// Wait for producers
	for i := 0; i < 5; i++ {
		<-done
	}

	// Drain the stack
	count := 0
	for {
		_, ok := stack.TryRecv()
		if !ok {
			break
		}
		count++
	}

	// With 100 capacity and 100 pushes, should have at most 100 items
	if count > 100 {
		t.Errorf("Expected at most 100 items, got %d", count)
	}

	if count == 0 {
		t.Error("Expected some items to be received")
	}
}
