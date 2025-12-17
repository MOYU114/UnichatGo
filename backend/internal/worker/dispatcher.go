package worker

import (
	"container/list"
	"sync"
	"time"
)

type userQueue struct {
	jobs     []Job
	enqueued bool
}

type Dispatcher struct {
	pool     *jobChannelPool
	JobQueue chan Job // interface for outer jobs get in the dispatcher
	Manager  *Manager

	mu        sync.Mutex
	queues    map[int64]*userQueue // job queue for each user
	ready     *list.List           // LRU queue storing user IDs
	positions map[int64]*list.Element
}

func NewDispatcher(minWorkers, maxWorkers, queueSize int, manager *Manager, idleTimeout time.Duration) *Dispatcher {
	pool := newJobChannelPool(minWorkers, maxWorkers, idleTimeout, manager)
	jobQueue := make(chan Job, queueSize)

	d := &Dispatcher{
		queues:    make(map[int64]*userQueue),
		ready:     list.New(),
		positions: make(map[int64]*list.Element),
		pool:      pool,
		JobQueue:  jobQueue,
		Manager:   manager,
	}

	// Warm up workers to keep previous behavior.
	for i := 0; i < minWorkers; i++ {
		d.pool.spawnWorker()
	}

	go d.run()
	return d
}

func (d *Dispatcher) run() {
	for {
		// dispatch one job of user in the front of LRU queue
		if !d.dispatchOne() {
			job := <-d.JobQueue // force congestion
			d.enqueueJob(job)
			continue
		}
		// if we have a new job, enqueue it and its caller user
		select {
		case job := <-d.JobQueue: // non-congestion
			d.enqueueJob(job)
		default:
		}
	}
}

func (d *Dispatcher) CancelUser(userID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.queues, userID)
	if elem, ok := d.positions[userID]; ok {
		d.ready.Remove(elem)
		delete(d.positions, userID)
	}
}

func (d *Dispatcher) enqueueJob(job Job) {
	userID := job.userID()

	d.mu.Lock()
	defer d.mu.Unlock()

	q := d.queues[userID]
	if q == nil {
		q = &userQueue{}
		d.queues[userID] = q
	}
	q.jobs = append(q.jobs, job)
	if q.enqueued {
		// user already enqueue, skip
		return
	}
	// new user, enqueue
	q.enqueued = true
	elem := d.ready.PushBack(userID)
	d.positions[userID] = elem
}

// dispatchOne get first user in LRU and dispatch its job
func (d *Dispatcher) dispatchOne() bool {
	d.mu.Lock()
	elem := d.ready.Front()
	for elem != nil {
		userID := elem.Value.(int64)
		q := d.queues[userID]
		// get job from the first user
		job := q.jobs[0]
		q.jobs = q.jobs[1:]
		if len(q.jobs) == 0 {
			// user only have one job, it'll be handled, user needs to quit queue
			q.enqueued = false
			d.ready.Remove(elem)
			delete(d.positions, userID)
		} else {
			// get to the back of queue
			d.ready.MoveToBack(elem)
		}
		d.mu.Unlock()

		workerChan := d.pool.acquire()
		workerID := d.pool.workerID(workerChan)
		debugLog("[dispatcher] assign job %s for user %d to worker-%d", job.Type, userID, workerID)
		workerChan <- job
		return true
	}
	d.mu.Unlock()
	return false
}

func (job Job) userID() int64 {
	switch job.Type {
	case Init:
		return job.SessionTask.req.UserID
	case Stream:
		return job.StreamTask.req.UserID
	default:
		return 0
	}
}
