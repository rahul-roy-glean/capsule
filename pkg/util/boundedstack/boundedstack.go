// Package boundedstack provides a concurrent-safe bounded stack that evicts
// oldest items when capacity is reached. It's designed for eager prefetching
// where we want to prioritize recently requested items.
package boundedstack

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrClosed is returned when operations are attempted on a closed stack.
	ErrClosed = errors.New("bounded stack is closed")
)

// BoundedStack is a concurrent-safe bounded stack.
// When the stack is full, pushing a new item evicts the oldest item.
// This prioritizes recently added items for eager prefetching.
type BoundedStack[T any] struct {
	items    []T
	capacity int
	head     int // Index of next push location
	size     int // Current number of items
	mu       sync.Mutex
	cond     *sync.Cond
	closed   bool
}

// New creates a new BoundedStack with the given capacity.
func New[T any](capacity int) (*BoundedStack[T], error) {
	if capacity <= 0 {
		return nil, errors.New("capacity must be positive")
	}
	s := &BoundedStack[T]{
		items:    make([]T, capacity),
		capacity: capacity,
	}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

// Push adds an item to the stack. If the stack is full, the oldest item is evicted.
// Push never blocks.
func (s *BoundedStack[T]) Push(item T) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	// Add item at head position
	s.items[s.head] = item
	s.head = (s.head + 1) % s.capacity

	if s.size < s.capacity {
		s.size++
	}

	// Signal any waiting receivers
	s.cond.Signal()
}

// Recv receives an item from the stack, blocking until an item is available
// or the context is cancelled or the stack is closed.
func (s *BoundedStack[T]) Recv(ctx context.Context) (T, error) {
	var zero T

	// Set up context cancellation
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	s.mu.Lock()
	defer s.mu.Unlock()

	for s.size == 0 && !s.closed {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		s.cond.Wait()
	}

	if s.closed && s.size == 0 {
		return zero, ErrClosed
	}

	if ctx.Err() != nil {
		return zero, ctx.Err()
	}

	// Pop from head (most recently added)
	s.head = (s.head - 1 + s.capacity) % s.capacity
	s.size--
	item := s.items[s.head]

	return item, nil
}

// TryRecv attempts to receive an item without blocking.
// Returns the item and true if successful, or zero value and false if empty.
func (s *BoundedStack[T]) TryRecv() (T, bool) {
	var zero T

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.size == 0 || s.closed {
		return zero, false
	}

	s.head = (s.head - 1 + s.capacity) % s.capacity
	s.size--
	return s.items[s.head], true
}

// Close closes the stack. Any blocked Recv calls will return ErrClosed.
func (s *BoundedStack[T]) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.cond.Broadcast()
}

// Len returns the current number of items in the stack.
func (s *BoundedStack[T]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}
