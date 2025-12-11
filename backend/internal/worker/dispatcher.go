package worker

import (
	"container/list"
	"sync"
)

const maxQueueSize = 100

type userQueue struct {
	jobs     []Job
	enqueued bool
}

type Dispatcher struct {
	WorkerPool chan chan Job
	JobQueue   chan Job // interface for outer jobs get in the dispatcher
	Workers    []*Worker

	mu     sync.Mutex
	queues map[int64]*userQueue //job queue for each user
	// lru: handle user's job dispatch order
	ready     *list.List              //a lru queue to get userid
	positions map[int64]*list.Element // hash map to save userid's pos, for de
}

func NewDispatcher(maxWorkers, queueSize int, manager *Manager) *Dispatcher {
	pool := make(chan chan Job, maxWorkers)
	jobQueue := make(chan Job, queueSize)
	workers := make([]*Worker, maxWorkers)

	d := &Dispatcher{
		queues:     make(map[int64]*userQueue),
		ready:      list.New(),
		positions:  make(map[int64]*list.Element),
		WorkerPool: pool,
		JobQueue:   jobQueue,
	}

	for i := 0; i < maxWorkers; i++ {
		worker := NewWorker(pool, manager)
		worker.Start()
		workers[i] = worker
	}
	d.Workers = workers

	go d.run()
	return d
}

func (d *Dispatcher) run() {
	for {
		// dispatch one job of user in the front of lru queue
		if !d.dispatchOne() {
			job := <-d.JobQueue
			d.enqueueJob(job)
			continue
		}
		// if we have a new job, enqueue it and its caller user
		select {
		case job := <-d.JobQueue:
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
		return
	}
	q.enqueued = true
	elem := d.ready.PushBack(userID)
	d.positions[userID] = elem
}

// dispatchOne get first user in lru and dispatch its job
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
			// user only have one job, it'll be handled, user need quit queue
			q.enqueued = false
			d.ready.Remove(elem)
			delete(d.positions, userID)
		} else {
			// get to the back of queue
			d.ready.MoveToBack(elem)
		}
		d.mu.Unlock()

		// give a job to the worker in the pool
		workerChan := <-d.WorkerPool
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
