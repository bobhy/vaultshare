package syncengine

import (
	"context"
	"slices"
	"sort"
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

// TaskKind identifies the operation a worker executes for a queued SyncTask.
type TaskKind int

const (
	// TaskSync runs full 3-way reconciliation for SyncTask.Path.
	TaskSync TaskKind = iota
	// TaskLocalRename mirrors a local file rename (OldPath -> Path) to Drive and Cache.
	TaskLocalRename
	// TaskLocalFolderRename mirrors a local folder rename (OldPath -> Path) to Drive and Cache.
	TaskLocalFolderRename
	// TaskTrashEmptyFolder trashes an empty Drive folder whose local folder was deleted.
	TaskTrashEmptyFolder
)

// SyncTask represents a single queued sync or filesystem-mirroring operation.
type SyncTask struct {
	Kind       TaskKind
	Path       string
	OldPath    string
	Priority   Priority
	Source     string
	EnqueuedAt time.Time
	BatchIDs   []uint64
}

func (t *SyncTask) lockPaths() []string {
	if t.OldPath != "" && t.OldPath != t.Path {
		return []string{t.OldPath, t.Path}
	}
	if t.Path != "" {
		return []string{t.Path}
	}
	return nil
}

// workQueue implements a strict priority queue with FIFO ordered action tasks,
// queued-only path deduplication for TaskSync, and active in-flight guards.
type workQueue struct {
	mu         sync.Mutex
	actionLane []*SyncTask
	highLane   []string
	bulkLane   []string
	queued     map[string]*SyncTask
	inFlight   map[string]*SyncTask
	wakeCh     chan struct{}
	drainCh    chan struct{}
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

func (q *workQueue) notifyDrainIfEmptyLocked() {
	if q.highCountLocked() == 0 {
		close(q.drainCh)
		q.drainCh = make(chan struct{})
	}
}

func (q *workQueue) WakeChan() <-chan struct{} {
	q.mu.Lock()
	ch := q.wakeCh
	q.mu.Unlock()
	return ch
}

// Enqueue adds a TaskSync or merges into an already-queued TaskSync for the same path.
// In-flight paths are not in queued, so new events arriving mid-execution queue once.
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
		Kind:       TaskSync,
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

// EnqueueAction appends an ordered High-priority filesystem action (rename or
// empty-folder trash) that executes FIFO ahead of regular sync tasks.
func (q *workQueue) EnqueueAction(task SyncTask) {
	q.mu.Lock()
	defer q.mu.Unlock()
	task.Priority = PriorityHigh
	copied := task
	q.actionLane = append(q.actionLane, &copied)
	q.notifyLocked()
}

// PopNext returns the highest-priority runnable task whose target path(s) are
// not currently inFlight.
func (q *workQueue) PopNext(blocked bool) (*SyncTask, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if blocked {
		return nil, false
	}
	if task, ok := q.popActionLocked(); ok {
		return task, true
	}
	// Preserve strict FIFO ordering for pending actions affecting subsequent work.
	if len(q.actionLane) > 0 {
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

func (q *workQueue) popActionLocked() (*SyncTask, bool) {
	if len(q.actionLane) == 0 {
		return nil, false
	}
	// Actions execute strictly FIFO (e.g. rename A->B must complete before rename B->C).
	head := q.actionLane[0]
	for _, p := range head.lockPaths() {
		if _, busy := q.inFlight[p]; busy {
			return nil, false
		}
	}
	q.actionLane = q.actionLane[1:]
	for _, p := range head.lockPaths() {
		q.inFlight[p] = head
	}
	return head, true
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
	q.notifyDrainIfEmptyLocked()
	q.mu.Unlock()
}

// FinishTask releases all paths held inFlight by task and wakes waiters.
func (q *workQueue) FinishTask(task *SyncTask) {
	if task == nil {
		return
	}
	q.mu.Lock()
	for _, p := range task.lockPaths() {
		if cur, ok := q.inFlight[p]; ok && cur == task {
			delete(q.inFlight, p)
		}
	}
	q.notifyLocked()
	q.notifyDrainIfEmptyLocked()
	q.mu.Unlock()
}

// Clear drops all pending queued tasks (used after stopping an epoch on reset).
func (q *workQueue) Clear() {
	q.mu.Lock()
	q.actionLane = nil
	q.highLane = nil
	q.bulkLane = nil
	clear(q.queued)
	clear(q.inFlight)
	q.notifyLocked()
	q.notifyDrainIfEmptyLocked()
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
	high = len(q.highLane) + len(q.actionLane)
	bulk = len(q.bulkLane)
	seen := make(map[*SyncTask]bool, len(q.inFlight))
	for _, t := range q.inFlight {
		seen[t] = true
	}
	inFlight = len(seen)
	q.mu.Unlock()
	return high, bulk, inFlight
}

// IsInFlight reports whether a task touching path is currently running on a worker.
func (q *workQueue) IsInFlight(path string) bool {
	q.mu.Lock()
	_, busy := q.inFlight[path]
	q.mu.Unlock()
	return busy
}

// pathLocker serializes all async mutations (workers, RPC conflict actions, and
// rename handlers) on the same vault path without holding any sync.Mutex across I/O.
type pathLocker struct {
	mu     sync.Mutex
	active map[string]chan struct{}
}

func newPathLocker() *pathLocker {
	return &pathLocker{active: make(map[string]chan struct{})}
}

// IsBusy reports whether any mutator currently holds the lock for path.
func (l *pathLocker) IsBusy(path string) bool {
	l.mu.Lock()
	_, busy := l.active[path]
	l.mu.Unlock()
	return busy
}

func (l *pathLocker) Acquire(ctx context.Context, paths ...string) (func(), error) {
	unique := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p != "" && !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	if len(unique) == 0 {
		return func() {}, nil
	}
	sort.Strings(unique)

	released := false
	acquired := make([]string, 0, len(unique))
	releaseAll := func() {
		if released {
			return
		}
		released = true
		l.mu.Lock()
		for i := len(acquired) - 1; i >= 0; i-- {
			p := acquired[i]
			if ch, ok := l.active[p]; ok {
				delete(l.active, p)
				close(ch)
			}
		}
		l.mu.Unlock()
	}

	for _, p := range unique {
		for {
			l.mu.Lock()
			waitCh, busy := l.active[p]
			if !busy {
				l.active[p] = make(chan struct{})
				acquired = append(acquired, p)
				l.mu.Unlock()
				break
			}
			l.mu.Unlock()

			select {
			case <-ctx.Done():
				releaseAll()
				return nil, ctx.Err()
			case <-waitCh:
			}
		}
	}
	return releaseAll, nil
}

func (q *workQueue) highCountLocked() int {
	n := len(q.highLane) + len(q.actionLane)
	seen := make(map[*SyncTask]bool, len(q.inFlight))
	for _, t := range q.inFlight {
		if t.Priority == PriorityHigh && !seen[t] {
			seen[t] = true
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
