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
	workerPool chan chan Job
	jobChannel chan Job
	quit       chan struct{}
}

func NewWorker(pool chan chan Job, manager *Manager) *Worker {
	return &Worker{
		manager:    manager,
		workerPool: pool,
		jobChannel: make(chan Job),
		quit:       make(chan struct{}),
	}
}

func (w *Worker) Start() {
	go func() {
		for {
			w.workerPool <- w.jobChannel
			select {
			case job := <-w.jobChannel:
				switch job.Type {
				case Init:
					w.manager.handleInit(job.SessionTask)
				case Stream:
					w.manager.handleStream(job.StreamTask)
				}
			case <-w.quit:
				return
			}
		}
	}()
}

func (w *Worker) Stop() {
	close(w.quit)
}
