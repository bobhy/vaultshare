package syncengine

import (
	"slices"
	"sync"
	"time"
)

// Priority separates user-facing live events from bulk Full Scan work.
type Priority int

const (
	// PriorityHigh preempts PriorityBulk for live edits, file-open, and Drive changes.
	PriorityHigh Priority = iota
	// PriorityBulk is used for Full Scan discovery and audits.
	PriorityBulk
)

// SyncTask represents a single path-scoped sync operation in the work queue.
type SyncTask struct {
	Path       string
	Priority   Priority
	Source     string
	EnqueuedAt time.Time
	BatchIDs   []uint64
}

// workQueue implements a strict two-lane priority queue with queued-only path
// deduplication and an active inFlight execution guard.
type workQueue struct {
	mu       sync.Mutex
	highLane []string
	bulkLane []string
	queued   map[string]*SyncTask
	inFlight map[string]*SyncTask
	wakeCh   chan struct{}
	drainCh  chan struct{}
}

func newWorkQueue() *workQueue {
	return &workQueue{
		queued:   make(map[string]*SyncTask),
		inFlight: make(map[string]*SyncTask),
		wakeCh:   make(chan struct{}),
		drainCh:  make(chan struct{}),
	}
}

func (q *workQueue) notifyLocked() {
	close(q.wakeCh)
	q.wakeCh = make(chan struct{})
}

func (q *workQueue) WakeChan() <-chan struct{} {
	q.mu.Lock()
	ch := q.wakeCh
	q.mu.Unlock()
	return ch
}

// Enqueue adds a task or merges into an already-queued entry for the same path.
// In-flight tasks are not in queued, so new events arriving mid-execution queue once.
func (q *workQueue) Enqueue(path string, pri Priority, source string, now time.Time, batchID uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if existing, ok := q.queued[path]; ok {
		if pri == PriorityHigh && existing.Priority == PriorityBulk {
			existing.Priority = PriorityHigh
			existing.Source = source
			existing.EnqueuedAt = now
			q.bulkLane = removePath(q.bulkLane, path)
			q.highLane = append(q.highLane, path)
		}
		if batchID != 0 && !slices.Contains(existing.BatchIDs, batchID) {
			existing.BatchIDs = append(existing.BatchIDs, batchID)
		}
		q.notifyLocked()
		return
	}

	task := &SyncTask{
		Path:       path,
		Priority:   pri,
		Source:     source,
		EnqueuedAt: now,
	}
	if batchID != 0 {
		task.BatchIDs = []uint64{batchID}
	}
	q.queued[path] = task
	if pri == PriorityHigh {
		q.highLane = append(q.highLane, path)
	} else {
		q.bulkLane = append(q.bulkLane, path)
	}
	q.notifyLocked()
}

// PopNext returns the highest-priority queued path not currently inFlight.
func (q *workQueue) PopNext(blocked bool) (*SyncTask, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if blocked {
		return nil, false
	}
	if task, ok := q.popFromLaneLocked(&q.highLane); ok {
		return task, true
	}
	// Strict lane separation: never pop Bulk while any High task is waiting.
	if len(q.highLane) > 0 {
		return nil, false
	}
	return q.popFromLaneLocked(&q.bulkLane)
}

func (q *workQueue) popFromLaneLocked(lane *[]string) (*SyncTask, bool) {
	for i, p := range *lane {
		if _, busy := q.inFlight[p]; busy {
			continue
		}
		task := q.queued[p]
		delete(q.queued, p)
		*lane = append((*lane)[:i], (*lane)[i+1:]...)
		q.inFlight[p] = task
		return task, true
	}
	return nil, false
}

// Finish marks path as no longer inFlight and wakes dispatchers / drain waiters.
func (q *workQueue) Finish(path string) {
	q.mu.Lock()
	delete(q.inFlight, path)
	q.notifyLocked()
	if q.highCountLocked() == 0 {
		close(q.drainCh)
		q.drainCh = make(chan struct{})
	}
	q.mu.Unlock()
}

// Wake wakes any parked workers (e.g., on Resume or Shutdown).
func (q *workQueue) Wake() {
	q.mu.Lock()
	q.notifyLocked()
	q.mu.Unlock()
}

// Counts returns (highQueued, bulkQueued, inFlight).
func (q *workQueue) Counts() (high, bulk, inFlight int) {
	q.mu.Lock()
	high = len(q.highLane)
	bulk = len(q.bulkLane)
	inFlight = len(q.inFlight)
	q.mu.Unlock()
	return high, bulk, inFlight
}

func (q *workQueue) highCountLocked() int {
	n := len(q.highLane)
	for _, t := range q.inFlight {
		if t.Priority == PriorityHigh {
			n++
		}
	}
	return n
}

// HighDrained returns whether high queue + high inFlight are empty, plus a channel
// that closes when they next drain.
func (q *workQueue) HighDrained() (bool, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.highCountLocked() == 0 {
		return true, nil
	}
	return false, q.drainCh
}

func removePath(slice []string, target string) []string {
	for i, s := range slice {
		if s == target {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}
