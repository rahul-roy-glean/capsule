package boundedstack

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	// Test valid capacity
	s, err := New[int](10)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if s == nil {
		t.Fatal("New() returned nil")
	}

	// Test invalid capacity
	_, err = New[int](0)
	if err == nil {
		t.Fatal("New(0) should fail")
	}

	_, err = New[int](-1)
	if err == nil {
		t.Fatal("New(-1) should fail")
	}
}

func TestPushRecv(t *testing.T) {
	s, _ := New[int](5)
	ctx := context.Background()

	// Push and receive
	s.Push(1)
	s.Push(2)
	s.Push(3)

	// Should receive in LIFO order (stack)
	val, err := s.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv() failed: %v", err)
	}
	if val != 3 {
		t.Errorf("Expected 3, got %d", val)
	}

	val, err = s.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv() failed: %v", err)
	}
	if val != 2 {
		t.Errorf("Expected 2, got %d", val)
	}

	val, err = s.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv() failed: %v", err)
	}
	if val != 1 {
		t.Errorf("Expected 1, got %d", val)
	}
}

func TestEviction(t *testing.T) {
	s, _ := New[int](3)
	ctx := context.Background()

	// Push more than capacity
	s.Push(1)
	s.Push(2)
	s.Push(3)
	s.Push(4) // Should evict 1
	s.Push(5) // Should evict 2

	if s.Len() != 3 {
		t.Errorf("Expected len 3, got %d", s.Len())
	}

	// Should receive 5, 4, 3 (not 1, 2)
	val, _ := s.Recv(ctx)
	if val != 5 {
		t.Errorf("Expected 5, got %d", val)
	}

	val, _ = s.Recv(ctx)
	if val != 4 {
		t.Errorf("Expected 4, got %d", val)
	}

	val, _ = s.Recv(ctx)
	if val != 3 {
		t.Errorf("Expected 3, got %d", val)
	}
}

func TestTryRecv(t *testing.T) {
	s, _ := New[int](5)

	// Empty stack
	_, ok := s.TryRecv()
	if ok {
		t.Error("TryRecv on empty stack should return false")
	}

	// With items
	s.Push(42)
	val, ok := s.TryRecv()
	if !ok {
		t.Error("TryRecv should return true")
	}
	if val != 42 {
		t.Errorf("Expected 42, got %d", val)
	}

	// Empty again
	_, ok = s.TryRecv()
	if ok {
		t.Error("TryRecv on empty stack should return false")
	}
}

func TestClose(t *testing.T) {
	s, _ := New[int](5)

	s.Push(1)
	s.Close()

	// Push after close should be no-op
	s.Push(2)

	// Recv after close should return remaining items then error
	ctx := context.Background()
	val, err := s.Recv(ctx)
	if err != nil {
		t.Fatalf("First Recv after close should return item: %v", err)
	}
	if val != 1 {
		t.Errorf("Expected 1, got %d", val)
	}

	_, err = s.Recv(ctx)
	if err != ErrClosed {
		t.Errorf("Expected ErrClosed, got %v", err)
	}
}

func TestRecvBlocking(t *testing.T) {
	s, _ := New[int](5)
	ctx := context.Background()

	var wg sync.WaitGroup
	var received int

	wg.Add(1)
	go func() {
		defer wg.Done()
		val, err := s.Recv(ctx)
		if err != nil {
			t.Errorf("Recv failed: %v", err)
			return
		}
		received = val
	}()

	// Give goroutine time to block
	time.Sleep(10 * time.Millisecond)

	// Push should unblock receiver
	s.Push(99)

	wg.Wait()

	if received != 99 {
		t.Errorf("Expected 99, got %d", received)
	}
}

func TestRecvContextCancellation(t *testing.T) {
	s, _ := New[int](5)
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var recvErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, recvErr = s.Recv(ctx)
	}()

	// Give goroutine time to block
	time.Sleep(10 * time.Millisecond)

	// Cancel should unblock receiver
	cancel()

	wg.Wait()

	if recvErr != context.Canceled {
		t.Errorf("Expected context.Canceled, got %v", recvErr)
	}
}

func TestConcurrentPushRecv(t *testing.T) {
	s, _ := New[int](100)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const numProducers = 10
	const numItems = 100

	var wg sync.WaitGroup

	// Start producers
	for i := 0; i < numProducers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numItems; j++ {
				s.Push(id*numItems + j)
			}
		}(i)
	}

	// Wait for producers to finish
	wg.Wait()

	// Collect items using TryRecv (non-blocking)
	received := make([]int, 0)
	for {
		val, ok := s.TryRecv()
		if !ok {
			break
		}
		received = append(received, val)
	}

	// With bounded stack of 100 and 1000 pushes, we should have at most 100 items
	if len(received) > 100 {
		t.Errorf("Expected at most 100 items, got %d", len(received))
	}

	// Should have received some items
	if len(received) == 0 {
		t.Error("Should have received some items")
	}

	// Use ctx to avoid unused variable warning
	_ = ctx
}

func TestLen(t *testing.T) {
	s, _ := New[int](10)

	if s.Len() != 0 {
		t.Errorf("Expected len 0, got %d", s.Len())
	}

	s.Push(1)
	if s.Len() != 1 {
		t.Errorf("Expected len 1, got %d", s.Len())
	}

	s.Push(2)
	s.Push(3)
	if s.Len() != 3 {
		t.Errorf("Expected len 3, got %d", s.Len())
	}

	s.TryRecv()
	if s.Len() != 2 {
		t.Errorf("Expected len 2, got %d", s.Len())
	}
}
