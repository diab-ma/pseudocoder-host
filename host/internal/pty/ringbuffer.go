// Package pty provides PTY (pseudo-terminal) session management.
// A PTY allows us to run commands as if they were in a real terminal,
// capturing their output including colors and formatting.
package pty

import (
	"sync"
)

// RingBuffer is a thread-safe circular buffer for terminal output lines.
//
// A ring buffer (also called circular buffer) is a fixed-size data structure
// that overwrites old data when full. Think of it like a circular track:
// when you reach the end, you loop back to the beginning.
//
// Example with capacity 3:
//
//	Write("A") -> [A, _, _]  head=1, size=1
//	Write("B") -> [A, B, _]  head=2, size=2
//	Write("C") -> [A, B, C]  head=0, size=3 (wrapped!)
//	Write("D") -> [D, B, C]  head=1, size=3 (A was overwritten)
//
// This is useful for terminal history because we only need the last N lines,
// and we don't want unbounded memory growth.
type RingBuffer struct {
	// mu is a mutex (mutual exclusion lock) that protects concurrent access.
	// RWMutex allows multiple readers OR one writer, but not both.
	// This is important because multiple goroutines might read/write simultaneously.
	mu sync.RWMutex

	// lines stores the actual data. It's pre-allocated to capacity size.
	lines []string

	// head points to where the NEXT write will go (not the newest item).
	// After a write, head advances by 1 (wrapping at capacity).
	head int

	// size tracks how many lines are currently stored (0 to cap).
	// Once the buffer is full, size stays at cap.
	size int

	// cap is the maximum number of lines we can store.
	cap int
}

// NewRingBuffer creates a new ring buffer with the given capacity.
// If capacity is <= 0, it defaults to 5000 lines.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 5000
	}
	// In Go, make() allocates and initializes slices, maps, and channels.
	// Here we create a string slice with 'capacity' elements, all empty strings.
	return &RingBuffer{
		lines: make([]string, capacity),
		cap:   capacity,
	}
}

// Write adds a line to the ring buffer.
// If the buffer is full, the oldest line is overwritten.
func (rb *RingBuffer) Write(line string) {
	// Lock() acquires exclusive write access. No other goroutine can read or write
	// until we call Unlock(). defer ensures Unlock() runs when the function exits,
	// even if there's a panic.
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Store the line at the current head position
	rb.lines[rb.head] = line

	// Advance head, wrapping around using modulo (%).
	// Example: if head=2 and cap=3, then (2+1)%3 = 0 (back to start)
	rb.head = (rb.head + 1) % rb.cap

	// Increase size until we reach capacity
	if rb.size < rb.cap {
		rb.size++
	}
}

// Lines returns all lines in the buffer, in order from oldest to newest.
// This creates a new slice, so the caller can safely use it without locking.
func (rb *RingBuffer) Lines() []string {
	// RLock() acquires a read lock. Multiple goroutines can hold read locks
	// simultaneously, but no writes can happen while any read lock is held.
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	// Allocate a new slice to return. This is a copy, so it's safe for the
	// caller to use even after we release the lock.
	result := make([]string, rb.size)
	if rb.size == 0 {
		return result
	}

	if rb.size < rb.cap {
		// Buffer not full yet: all lines are at the start of the array,
		// in order from index 0 to size-1.
		// copy() is a built-in function that copies elements between slices.
		copy(result, rb.lines[:rb.size])
	} else {
		// Buffer is full: head points to the oldest entry (it's where the
		// next write will overwrite). We need to read from head to end,
		// then from start to head-1.
		start := rb.head
		for i := 0; i < rb.size; i++ {
			// Use modulo to wrap around the array
			result[i] = rb.lines[(start+i)%rb.cap]
		}
	}

	return result
}

// Size returns the current number of lines in the buffer.
func (rb *RingBuffer) Size() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.size
}

// Capacity returns the maximum capacity of the buffer.
// This doesn't need locking because cap never changes after creation.
func (rb *RingBuffer) Capacity() int {
	return rb.cap
}

// Clear removes all lines from the buffer by resetting the counters.
// The underlying array still exists but will be overwritten on new writes.
func (rb *RingBuffer) Clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.head = 0
	rb.size = 0
}
