// Package inmemory provides an in-memory, goroutine-safe ring-buffer implementation
// of store.TimeSeriesStore.  It is the default store used when no external database
// is configured.  Data is lost on process restart; it is suitable for short-term
// anomaly detection windows (the last N snapshots).
package inmemory

import (
	"context"
	"sync"
	"time"

	"github.com/ochestra-tech/k8s-monitor/internal/ports/store"
)

// RingBuffer holds the last `capacity` MetricPoints per clusterID in a circular
// buffer.  When the buffer is full the oldest entry is overwritten.
type RingBuffer struct {
	mu       sync.RWMutex
	capacity int
	// per-cluster circular buffers
	bufs map[string]*clusterBuf
}

type clusterBuf struct {
	data  []store.MetricPoint
	head  int // next write position
	count int // number of valid entries (≤ capacity)
}

// NewRingBuffer creates a RingBuffer that keeps at most `capacity` points per cluster.
// capacity defaults to 1000 when ≤ 0.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &RingBuffer{
		capacity: capacity,
		bufs:     make(map[string]*clusterBuf),
	}
}

func (r *RingBuffer) getOrCreate(clusterID string) *clusterBuf {
	if cb, ok := r.bufs[clusterID]; ok {
		return cb
	}
	cb := &clusterBuf{data: make([]store.MetricPoint, r.capacity)}
	r.bufs[clusterID] = cb
	return cb
}

// Append stores a snapshot.  ctx is accepted but not used (the operation is always synchronous).
func (r *RingBuffer) Append(_ context.Context, pt store.MetricPoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cb := r.getOrCreate(pt.ClusterID)
	cb.data[cb.head] = pt
	cb.head = (cb.head + 1) % r.capacity
	if cb.count < r.capacity {
		cb.count++
	}
	return nil
}

// Latest returns the n most-recent snapshots for clusterID, oldest-first.
// If n <= 0 all stored points are returned.
func (r *RingBuffer) Latest(_ context.Context, clusterID string, n int) ([]store.MetricPoint, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cb, ok := r.bufs[clusterID]
	if !ok {
		return nil, nil
	}
	return extractOldestFirst(cb, n), nil
}

// QueryRange returns snapshots within [start, end] for clusterID.
func (r *RingBuffer) QueryRange(_ context.Context, clusterID string, start, end time.Time) ([]store.MetricPoint, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cb, ok := r.bufs[clusterID]
	if !ok {
		return nil, nil
	}
	all := extractOldestFirst(cb, 0)
	var out []store.MetricPoint
	for _, pt := range all {
		if (pt.Timestamp.Equal(start) || pt.Timestamp.After(start)) &&
			(pt.Timestamp.Equal(end) || pt.Timestamp.Before(end)) {
			out = append(out, pt)
		}
	}
	return out, nil
}

// extractOldestFirst returns up to n entries from cb in chronological order.
// Caller must hold r.mu (at least read-locked).
func extractOldestFirst(cb *clusterBuf, n int) []store.MetricPoint {
	if cb.count == 0 {
		return nil
	}
	total := cb.count
	if n > 0 && n < total {
		total = n
	}

	// Oldest entry: if buffer is full, head points to the oldest entry;
	// otherwise oldest is at index 0.
	capacity := len(cb.data)
	oldest := 0
	if cb.count == capacity {
		oldest = cb.head // head wraps to the oldest slot when full
	}

	out := make([]store.MetricPoint, total)
	// We want the last `total` entries (most recent `total` when n>0).
	skip := cb.count - total
	for i := 0; i < total; i++ {
		idx := (oldest + skip + i) % capacity
		out[i] = cb.data[idx]
	}
	return out
}
