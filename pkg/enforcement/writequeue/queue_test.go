package writequeue_test

import (
	"testing"
	"time"

	"iddc/pkg/enforcement/writequeue"
)

func TestQueue_EnqueueDrain(t *testing.T) {
	q := writequeue.NewQueue("svc", 10, time.Minute)

	for i := 0; i < 5; i++ {
		if err := q.Enqueue(writequeue.Write{ID: "w", Payload: []byte("x")}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if depth := q.Depth(); depth != 5 {
		t.Errorf("depth = %d, want 5", depth)
	}

	items := q.Drain()
	if len(items) != 5 {
		t.Errorf("drain returned %d items, want 5", len(items))
	}
	if q.Depth() != 0 {
		t.Errorf("depth after drain = %d, want 0", q.Depth())
	}
}

func TestQueue_MaxDepth(t *testing.T) {
	q := writequeue.NewQueue("svc", 3, time.Minute)

	for i := 0; i < 3; i++ {
		if err := q.Enqueue(writequeue.Write{ID: "ok"}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	// 4th enqueue must fail.
	if err := q.Enqueue(writequeue.Write{ID: "overflow"}); err == nil {
		t.Error("expected error when enqueuing past max depth")
	}
	if q.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", q.Dropped())
	}
}

func TestQueue_TTLEviction(t *testing.T) {
	ttl := 50 * time.Millisecond
	q := writequeue.NewQueue("svc", 100, ttl)

	if err := q.Enqueue(writequeue.Write{ID: "old"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(ttl + 20*time.Millisecond)

	// Depth() triggers eviction.
	if d := q.Depth(); d != 0 {
		t.Errorf("depth after TTL expiry = %d, want 0", d)
	}
	items := q.Drain()
	if len(items) != 0 {
		t.Errorf("drain after TTL returned %d items, want 0", len(items))
	}
}

func TestQueue_MaxDepth_ReturnedByMethod(t *testing.T) {
	q := writequeue.NewQueue("svc", 42, time.Minute)
	if q.MaxDepth() != 42 {
		t.Errorf("MaxDepth = %d, want 42", q.MaxDepth())
	}
}

func TestQueue_DrainEmpty(t *testing.T) {
	q := writequeue.NewQueue("svc", 10, time.Minute)
	items := q.Drain()
	if items == nil {
		t.Error("Drain on empty queue should return non-nil slice")
	}
	if len(items) != 0 {
		t.Errorf("drain empty = %d items, want 0", len(items))
	}
}

func TestQueue_ConcurrentEnqueue(t *testing.T) {
	q := writequeue.NewQueue("svc", 1000, time.Minute)
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				_ = q.Enqueue(writequeue.Write{ID: "x", Payload: []byte("y")})
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	if q.Depth() > 1000 {
		t.Errorf("depth %d exceeds maxDepth 1000", q.Depth())
	}
}
