package worker

import (
	"sync"
	"time"
)

type workerMeta struct {
	ch        chan Job
	lastUsed  time.Time
	enqueued  bool // is in the idle queue
	discarded bool // is targeted as delete
}

type jobChannelPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	idle     []*workerMeta
	metadata map[chan Job]*workerMeta
	min      int
	max      int
	running  int
	expiry   time.Duration
	manager  *Manager
}

const defaultWorkerIdle = 30 * time.Second

func newJobChannelPool(minWorkers, maxWorkers int, idle time.Duration, manager *Manager) *jobChannelPool {
	if idle <= 0 {
		idle = defaultWorkerIdle
	}
	if maxWorkers < minWorkers {
		maxWorkers = minWorkers
	}
	p := &jobChannelPool{
		metadata: make(map[chan Job]*workerMeta),
		min:      minWorkers,
		max:      maxWorkers,
		expiry:   idle,
		manager:  manager,
	}
	p.cond = sync.NewCond(&p.mu)
	go p.purgeStaleWorkers()
	return p
}

// spawnWorker add a new worker, great for patch spawn
func (p *jobChannelPool) spawnWorker() {
	p.mu.Lock()
	if p.running >= p.max {
		p.mu.Unlock()
		return
	}
	worker := NewWorker(p, p.manager)
	meta := &workerMeta{
		ch: worker.jobChannel,
	}
	p.metadata[worker.jobChannel] = meta
	p.running++
	p.mu.Unlock()
	worker.Start()
}

// acquire get an idle worker, or spawn a new one
func (p *jobChannelPool) acquire() chan Job {
	for {
		p.mu.Lock()
		// get an idle worker
		if meta := p.popIdleLocked(); meta != nil {
			p.mu.Unlock()
			return meta.ch
		}
		// need to add a new worker, spawn one (can't call spawnWorker because the p.mu)
		if p.running < p.max {
			worker := NewWorker(p, p.manager)
			meta := &workerMeta{ch: worker.jobChannel}
			p.metadata[worker.jobChannel] = meta
			p.running++
			p.mu.Unlock()
			worker.Start()
			continue
		}
		p.cond.Wait()
		p.mu.Unlock()
	}
}

// Release add an idle worker into the pool
func (p *jobChannelPool) Release(ch chan Job) {
	p.mu.Lock()
	meta, ok := p.metadata[ch]
	if !ok || meta.discarded || meta.enqueued {
		p.mu.Unlock()
		return
	}
	meta.enqueued = true
	meta.lastUsed = time.Now()
	p.idle = append(p.idle, meta)
	p.mu.Unlock()
	p.cond.Signal()
}

// retire delete a worker
func (p *jobChannelPool) retire(ch chan Job) {
	p.mu.Lock()
	if meta, ok := p.metadata[ch]; ok {
		delete(p.metadata, ch)
		meta.discarded = true
		if p.running > 0 {
			p.running--
		}
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// popIdleLocked check if pool has an idle worker, then return
func (p *jobChannelPool) popIdleLocked() *workerMeta {
	for len(p.idle) > 0 {
		meta := p.idle[0]
		p.idle = p.idle[1:]
		if meta.discarded {
			continue
		}
		meta.enqueued = false
		return meta
	}
	return nil
}

// purgeStaleWorkers call shutdownExpired when expiry time comes
func (p *jobChannelPool) purgeStaleWorkers() {
	ticker := time.NewTicker(p.expiry)
	defer ticker.Stop()
	for {
		<-ticker.C
		p.shutdownExpired()
	}
}

// shutdownExpired retire all the expired worker
func (p *jobChannelPool) shutdownExpired() {
	var stale []*workerMeta
	now := time.Now()

	p.mu.Lock()
	if len(p.idle) == 0 || p.running <= p.min {
		p.mu.Unlock()
		return
	}
	remaining := p.idle[:0] // keep the original array
	for _, meta := range p.idle {
		if meta.discarded { // discarded currently deleting worker
			continue
		}
		if now.Sub(meta.lastUsed) >= p.expiry && p.running-len(stale) > p.min {
			meta.discarded = true
			meta.enqueued = false
			stale = append(stale, meta) // into the stale array, will delete
			continue
		}
		remaining = append(remaining, meta) // into the remaining array
	}
	p.idle = remaining
	p.mu.Unlock()

	for _, meta := range stale {
		meta.ch <- Job{Type: Stop}
	}
}
