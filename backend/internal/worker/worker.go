package worker

import (
	"sync/atomic"

	"unichatgo/internal/models"
)

type workerReturn struct {
	session   *models.Session
	aiMessage *models.Message
	title     string
	err       error
}

type Worker struct {
	manager    *Manager
	pool       *jobChannelPool
	jobChannel chan Job
	id         int64
}

var workerSeq int64

func NewWorker(pool *jobChannelPool, manager *Manager) *Worker {
	return &Worker{
		manager:    manager,
		pool:       pool,
		jobChannel: make(chan Job),
		id:         atomic.AddInt64(&workerSeq, 1),
	}
}

func (w *Worker) Start() {
	go func() {
		for {
			w.pool.MarkIdle(w.jobChannel)
			job := <-w.jobChannel
			debugLog("[worker-%d] accepted job type=%s", w.id, job.Type)
			switch job.Type {
			case Init:
				w.manager.handleInit(job.SessionTask)
			case Stream:
				w.manager.handleStream(job.StreamTask)
			case Stop:
				debugLog("[worker-%d] stopping", w.id)
				w.pool.retire(w.jobChannel)
				return
			}
		}
	}()
}
