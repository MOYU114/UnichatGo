package worker

import (
	"sync"
	"sync/atomic"
	"time"
)

type workerMeta struct {
	ch        chan Job
	lastUsed  time.Time
	enqueued  bool // is in the idle queue
	discarded bool // is targeted as delete
	id        int64
}

type jobChannelPool struct {
	mu            sync.Mutex
	cond          *sync.Cond
	idle          []*workerMeta
	metadata      map[chan Job]*workerMeta
	minBase       int
	max           int
	running       int
	expiry        time.Duration
	manager       *Manager
	minDynamic    atomic.Int32 // dynamic worker numbers
	minBoostUntil atomic.Int64
	minRetention  time.Duration
}

const (
	defaultWorkerIdle   = 30 * time.Second
	defaultMinRetention = time.Minute
	minDecaySlack       = 1
)

func newJobChannelPool(minWorkers, maxWorkers int, idleTime time.Duration, manager *Manager) *jobChannelPool {
	if idleTime <= 0 {
		idleTime = defaultWorkerIdle
	}
	if maxWorkers < minWorkers {
		maxWorkers = minWorkers
	}
	retainTime := idleTime
	if retainTime < defaultMinRetention {
		retainTime = defaultMinRetention
	}
	p := &jobChannelPool{
		metadata:     make(map[chan Job]*workerMeta),
		minBase:      minWorkers,
		max:          maxWorkers,
		expiry:       idleTime,
		manager:      manager,
		minRetention: retainTime,
	}
	p.minDynamic.Store(int32(minWorkers))
	p.cond = sync.NewCond(&p.mu)
	go p.purgeStaleWorkers()
	return p
}

// currentBoundary get current worker boundary
func (p *jobChannelPool) currentBoundary() int {
	//dyn := int(p.minDynamic.Load())
	//if dyn < p.minBase {
	//	return p.minBase
	//}
	//return dyn
	return int(p.minDynamic.Load())
}

// tryBoostBoundary try to boost min worker boundary, called when running++
func (p *jobChannelPool) tryBoostBoundary(running int) {
	curBoundary := p.currentBoundary()
	if running <= curBoundary {
		return
	}
	// running worker higher than the boundary, means busy;
	// running higher than curBoundary, up to then running, till running not higher than curBoundary
	for {
		curBoundary = p.currentBoundary()
		if running <= curBoundary {
			break
		}
		if p.minDynamic.CompareAndSwap(int32(curBoundary), int32(running)) {
			p.minBoostUntil.Store(time.Now().Add(p.minRetention).UnixNano())
			break
		}
	}
}

func (p *jobChannelPool) decayBoundary(now time.Time) {
	deadline := p.minBoostUntil.Load()
	if deadline == 0 || now.UnixNano() < deadline {
		return
	}
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()

	curBoundary := p.currentBoundary()
	// if running higher than curBoundary-epsilon, means high load, need to keep current boundary
	// just reset clock, tryBoostBoundary will do the work
	if running >= curBoundary-minDecaySlack {
		p.minBoostUntil.Store(now.Add(p.minRetention).UnixNano())
		return
	}

	// curRunning boundary is greater than minBase
	curRunning := running
	if curRunning < p.minBase {
		curRunning = p.minBase
	}
	for {
		curBoundary = p.currentBoundary()
		if curBoundary <= curRunning {
			if curRunning == p.minBase {
				p.minBoostUntil.Store(0) // stop boost clock
			} else {
				p.minBoostUntil.Store(now.Add(p.minRetention).UnixNano())
			}
			return
		}
		// curBoundary > curRunning, decay minDynamic into min(curRunning,minBase)
		if p.minDynamic.CompareAndSwap(int32(curBoundary), int32(curRunning)) {
			if curRunning == p.minBase {
				p.minBoostUntil.Store(0)
			} else {
				p.minBoostUntil.Store(now.Add(p.minRetention).UnixNano())
			}
			return
		}
	}
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
		id: worker.id,
	}
	p.metadata[worker.jobChannel] = meta
	p.running++
	p.tryBoostBoundary(p.running)
	debugLog("[worker-%d] spawn, running=%d", worker.id, p.running)
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
			meta := &workerMeta{ch: worker.jobChannel, id: worker.id}
			p.metadata[worker.jobChannel] = meta
			p.running++
			p.tryBoostBoundary(p.running)
			debugLog("[worker-%d] dynamic spawn, running=%d", worker.id, p.running)
			p.mu.Unlock()
			worker.Start()
			continue
		}
		// currently running >= max, not allow to increase workers
		// now must wait a idle worker
		debugLog("[worker] waiting for idle worker, running=%d", p.running)
		p.cond.Wait()
		p.mu.Unlock()
	}
}

// MarkIdle add an idle worker into the pool
func (p *jobChannelPool) MarkIdle(ch chan Job) {
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

func (p *jobChannelPool) workerID(ch chan Job) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if meta, ok := p.metadata[ch]; ok {
		return meta.id
	}
	return 0
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
	for now := range ticker.C {
		// change boundary first, then shutdown
		p.decayBoundary(now)
		p.shutdownExpired(now)
	}
}

// shutdownExpired retire all the expired worker
func (p *jobChannelPool) shutdownExpired(now time.Time) {
	var stale []*workerMeta
	boundary := p.currentBoundary()

	p.mu.Lock()
	if len(p.idle) == 0 || p.running <= boundary {
		p.mu.Unlock()
		return
	}
	remaining := p.idle[:0] // keep the original array
	for _, meta := range p.idle {
		if meta.discarded { // discarded currently deleting worker
			continue
		}
		if now.Sub(meta.lastUsed) >= p.expiry && p.running-len(stale) > boundary {
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
