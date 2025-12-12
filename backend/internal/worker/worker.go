package worker

import "unichatgo/internal/models"

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
}

func NewWorker(pool *jobChannelPool, manager *Manager) *Worker {
	return &Worker{
		manager:    manager,
		pool:       pool,
		jobChannel: make(chan Job),
	}
}

func (w *Worker) Start() {
	go func() {
		for {
			w.pool.Release(w.jobChannel)
			job := <-w.jobChannel
			switch job.Type {
			case Init:
				w.manager.handleInit(job.SessionTask)
			case Stream:
				w.manager.handleStream(job.StreamTask)
			case Stop:
				w.pool.retire(w.jobChannel)
				return
			}
		}
	}()
}
