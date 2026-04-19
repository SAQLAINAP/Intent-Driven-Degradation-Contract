// Package writequeue implements a bounded write queue with TTL-based eviction.
// When the system is in a degraded tier that enables queuing, writes are buffered
// here instead of hitting the database directly.
package writequeue

import (
	"fmt"
	"sync"
	"time"

	"iddc/pkg/metrics"
)

// Write represents a single deferred write operation.
type Write struct {
	ID        string
	Payload   []byte
	EnqueuedAt time.Time
}

// Queue is a bounded, TTL-aware write buffer.
type Queue struct {
	mu       sync.Mutex
	items    []Write
	maxDepth int
	ttl      time.Duration
	dropped  int64
	service  string
}

// NewQueue creates a write queue with the given bounds.
func NewQueue(service string, maxDepth int, ttl time.Duration) *Queue {
	return &Queue{
		service:  service,
		maxDepth: maxDepth,
		ttl:      ttl,
	}
}

// Enqueue adds a write to the queue.
// Returns an error if the queue is at capacity (write is dropped).
func (q *Queue) Enqueue(w Write) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.evict()
	if len(q.items) >= q.maxDepth {
		q.dropped++
		metrics.WriteQueueDroppedTotal.WithLabelValues(q.service).Inc()
		return fmt.Errorf("write queue full (%d/%d): write dropped", len(q.items), q.maxDepth)
	}
	w.EnqueuedAt = time.Now()
	q.items = append(q.items, w)
	metrics.WriteQueueDepth.WithLabelValues(q.service).Set(float64(len(q.items)))
	return nil
}

// Drain returns all non-expired queued writes and clears the queue.
// The caller is responsible for replaying these against the real backend.
func (q *Queue) Drain() []Write {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.evict()
	out := make([]Write, len(q.items))
	copy(out, q.items)
	q.items = q.items[:0]
	return out
}

// Depth returns the current number of items in the queue.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.evict()
	return len(q.items)
}

// Dropped returns the total number of writes dropped due to overflow.
func (q *Queue) Dropped() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// MaxDepth returns the configured maximum queue depth.
func (q *Queue) MaxDepth() int {
	return q.maxDepth
}

// evict removes items that have exceeded their TTL. Must be called with mu held.
func (q *Queue) evict() {
	if q.ttl == 0 {
		return
	}
	cutoff := time.Now().Add(-q.ttl)
	newItems := q.items[:0]
	for _, item := range q.items {
		if item.EnqueuedAt.After(cutoff) {
			newItems = append(newItems, item)
		} else {
			q.dropped++
			metrics.WriteQueueDroppedTotal.WithLabelValues(q.service).Inc()
		}
	}
	q.items = newItems
}
