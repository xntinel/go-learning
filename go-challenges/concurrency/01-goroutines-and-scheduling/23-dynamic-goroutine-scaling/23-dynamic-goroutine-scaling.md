# Exercise 23: Dynamic Goroutine Scaling

A fixed-size worker pool is a lie. Configure 10 workers and let traffic spike
to 2,000 requests per second, and latency explodes because 1,990 requests sit
queued; configure 200 workers for the peak and let traffic fall to 10 requests
per second, and 190 idle goroutines waste memory and whatever resources they
hold. This exercise builds the alternative -- a pool that watches its own
metrics and resizes itself, the same feedback loop Kubernetes' Horizontal Pod
Autoscaler runs, but inside a single process.

## What you'll build

```text
23-dynamic-goroutine-scaling/
  main.go        a fixed pool with metrics, then an adaptive pool whose
                 control loop scales workers up and down under simulated load
```

- Build: an auto-scaling worker pool driven by queue depth and latency, complete with a variable-rate load generator.
- Implement: `PoolMetrics`/`Metrics` with atomic counters, `addWorker`/`removeWorker` over a per-worker stop channel, a `controlLoop` on a `time.Ticker`, and a `decide` step with independent scale-up and scale-down cooldowns.
- Verify: `go run main.go`

### Why scaling must be asymmetric

The solution is the same principle behind Kubernetes Horizontal Pod Autoscaler
(HPA), but inside a single process: a control loop monitors metrics (queue
depth, average latency), compares them to thresholds, and adjusts the worker
count. Scale up fast when queue depth spikes -- users are waiting. Scale down
slowly with a cooldown period -- traffic might spike again, and killing workers
you will need in 30 seconds causes thrashing.

This pattern appears in every high-throughput Go service: webhook processors,
message queue consumers, job schedulers, and API gateways. Building it teaches
you goroutine lifecycle management (creating workers, signaling them to stop,
waiting for them to drain), metrics-driven decision making, and the
coordination between a control goroutine and worker goroutines.


## Step 1 -- Basic Worker Pool with Metrics

Build a worker pool with a fixed size that tracks queue depth and processing latency. This is the foundation before adding dynamic scaling.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	initialWorkers  = 3
	totalJobs       = 30
	minProcessingMs = 20
	maxProcessingMs = 80
)

type PoolMetrics struct {
	Processed    int64
	TotalLatency int64
}

func (m *PoolMetrics) RecordJob(latency time.Duration) {
	atomic.AddInt64(&m.Processed, 1)
	atomic.AddInt64(&m.TotalLatency, int64(latency))
}

func (m *PoolMetrics) AvgLatency() time.Duration {
	processed := atomic.LoadInt64(&m.Processed)
	if processed == 0 {
		return 0
	}
	total := atomic.LoadInt64(&m.TotalLatency)
	return time.Duration(total / processed)
}

type WorkerPool struct {
	jobs    chan int
	metrics *PoolMetrics
	wg      sync.WaitGroup
	workers int
}

func NewWorkerPool(workerCount int, queueSize int) *WorkerPool {
	pool := &WorkerPool{
		jobs:    make(chan int, queueSize),
		metrics: &PoolMetrics{},
		workers: workerCount,
	}

	for i := 1; i <= workerCount; i++ {
		pool.wg.Add(1)
		go pool.runWorker(i)
	}

	return pool
}

func (p *WorkerPool) runWorker(id int) {
	defer p.wg.Done()

	for jobID := range p.jobs {
		start := time.Now()
		processingTime := time.Duration(minProcessingMs+rand.IntN(maxProcessingMs-minProcessingMs)) * time.Millisecond
		time.Sleep(processingTime)
		latency := time.Since(start)
		p.metrics.RecordJob(latency)
		fmt.Printf("  worker %d: job %d (%v)\n", id, jobID, latency.Round(time.Millisecond))
	}
}

func (p *WorkerPool) Submit(jobID int) {
	p.jobs <- jobID
}

func (p *WorkerPool) QueueDepth() int {
	return len(p.jobs)
}

func (p *WorkerPool) Shutdown() {
	close(p.jobs)
	p.wg.Wait()
}

func main() {
	pool := NewWorkerPool(initialWorkers, totalJobs)
	fmt.Printf("=== Worker Pool with Metrics ===\n")
	fmt.Printf("  Workers: %d, Queue capacity: %d\n\n", initialWorkers, totalJobs)

	for i := 1; i <= totalJobs; i++ {
		pool.Submit(i)
	}
	fmt.Printf("  Submitted %d jobs, queue depth: %d\n\n", totalJobs, pool.QueueDepth())

	pool.Shutdown()

	fmt.Printf("\n=== Results ===\n")
	fmt.Printf("  Processed: %d\n", atomic.LoadInt64(&pool.metrics.Processed))
	fmt.Printf("  Avg latency: %v\n", pool.metrics.AvgLatency().Round(time.Millisecond))
}
```

**What's happening here:** A fixed pool of 3 workers processes 30 jobs from a buffered channel. Each job takes a random duration to simulate variable processing time. `PoolMetrics` tracks how many jobs were processed and the total latency using atomic operations, allowing the metrics to be read from any goroutine safely.

**Key insight:** `QueueDepth()` returns the number of pending jobs in the channel buffer. This is the key metric for scaling decisions: if the queue is growing, we need more workers. If the queue is empty and workers are idle, we have too many.

### Verification
```bash
go run main.go
```
Expected output (order and latencies vary):
```
=== Worker Pool with Metrics ===
  Workers: 3, Queue capacity: 30

  Submitted 30 jobs, queue depth: 27

  worker 1: job 1 (45ms)
  worker 2: job 2 (67ms)
  worker 3: job 3 (33ms)
  worker 3: job 6 (52ms)
  worker 1: job 4 (71ms)
  ...
  worker 2: job 30 (38ms)

=== Results ===
  Processed: 30
  Avg latency: 51ms
```


## Step 2 -- Control Loop with Scaling Decisions

Add a control goroutine that monitors queue depth and worker count, then scales up or down based on thresholds.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	minWorkers       = 2
	maxWorkers       = 15
	queueCapacity    = 200
	scaleUpThreshold = 10
	scaleDownIdle    = 3 * time.Second
	controlInterval  = 500 * time.Millisecond
	minJobProcessMs  = 30
	maxJobProcessMs  = 100
)

type PoolMetrics struct {
	Processed    int64
	TotalLatency int64
}

func (m *PoolMetrics) RecordJob(latency time.Duration) {
	atomic.AddInt64(&m.Processed, 1)
	atomic.AddInt64(&m.TotalLatency, int64(latency))
}

type Worker struct {
	ID   int
	stop chan struct{}
}

type AdaptivePool struct {
	mu           sync.Mutex
	jobs         chan int
	metrics      *PoolMetrics
	workers      map[int]*Worker
	nextWorkerID int
	wg           sync.WaitGroup
	controlStop  chan struct{}
	lastBusyTime time.Time
}

func NewAdaptivePool() *AdaptivePool {
	pool := &AdaptivePool{
		jobs:         make(chan int, queueCapacity),
		metrics:      &PoolMetrics{},
		workers:      make(map[int]*Worker),
		controlStop:  make(chan struct{}),
		lastBusyTime: time.Now(),
	}

	for i := 0; i < minWorkers; i++ {
		pool.addWorker()
	}

	go pool.controlLoop()
	return pool
}

func (p *AdaptivePool) addWorker() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.workers) >= maxWorkers {
		return
	}

	p.nextWorkerID++
	w := &Worker{
		ID:   p.nextWorkerID,
		stop: make(chan struct{}),
	}
	p.workers[w.ID] = w
	p.wg.Add(1)

	go p.runWorker(w)
	fmt.Printf("    [scale] + worker %d (total: %d)\n", w.ID, len(p.workers))
}

func (p *AdaptivePool) removeWorker() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.workers) <= minWorkers {
		return
	}

	for id, w := range p.workers {
		close(w.stop)
		delete(p.workers, id)
		fmt.Printf("    [scale] - worker %d (total: %d)\n", id, len(p.workers))
		return
	}
}

func (p *AdaptivePool) runWorker(w *Worker) {
	defer p.wg.Done()

	for {
		select {
		case jobID, ok := <-p.jobs:
			if !ok {
				return
			}
			start := time.Now()
			processingTime := time.Duration(minJobProcessMs+rand.IntN(maxJobProcessMs-minJobProcessMs)) * time.Millisecond
			time.Sleep(processingTime)
			p.metrics.RecordJob(time.Since(start))
			fmt.Printf("  worker %d: job %d (%v)\n", w.ID, jobID, time.Since(start).Round(time.Millisecond))
		case <-w.stop:
			return
		}
	}
}

func (p *AdaptivePool) controlLoop() {
	ticker := time.NewTicker(controlInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			queueLen := len(p.jobs)
			p.mu.Lock()
			workerCount := len(p.workers)
			p.mu.Unlock()

			if queueLen > scaleUpThreshold {
				p.lastBusyTime = time.Now()
				needed := queueLen / scaleUpThreshold
				if needed > 3 {
					needed = 3
				}
				for i := 0; i < needed; i++ {
					p.addWorker()
				}
			} else if queueLen == 0 && time.Since(p.lastBusyTime) > scaleDownIdle {
				p.removeWorker()
			}

			fmt.Printf("  [control] queue=%d workers=%d processed=%d\n",
				queueLen, workerCount, atomic.LoadInt64(&p.metrics.Processed))

		case <-p.controlStop:
			return
		}
	}
}

func (p *AdaptivePool) Submit(jobID int) {
	p.jobs <- jobID
}

func (p *AdaptivePool) WorkerCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}

func (p *AdaptivePool) Shutdown() {
	close(p.controlStop)
	close(p.jobs)
	p.wg.Wait()
}

func main() {
	pool := NewAdaptivePool()
	fmt.Printf("=== Adaptive Pool with Control Loop ===\n")
	fmt.Printf("  Min workers: %d, Max workers: %d\n", minWorkers, maxWorkers)
	fmt.Printf("  Scale-up threshold: queue > %d\n", scaleUpThreshold)
	fmt.Printf("  Scale-down idle: %v\n\n", scaleDownIdle)

	fmt.Println("--- Phase 1: Burst load (50 jobs) ---")
	for i := 1; i <= 50; i++ {
		pool.Submit(i)
	}

	time.Sleep(3 * time.Second)

	fmt.Println()
	fmt.Println("--- Phase 2: Idle period ---")
	time.Sleep(5 * time.Second)

	fmt.Println()
	fmt.Println("--- Phase 3: Second burst (30 jobs) ---")
	for i := 51; i <= 80; i++ {
		pool.Submit(i)
	}

	time.Sleep(3 * time.Second)

	pool.Shutdown()
	fmt.Printf("\n=== Final ===\n")
	fmt.Printf("  Processed: %d jobs\n", atomic.LoadInt64(&pool.metrics.Processed))
}
```

**What's happening here:** The control loop runs every 500ms, reads the queue depth, and makes scaling decisions. If the queue exceeds `scaleUpThreshold`, it adds workers (up to 3 per interval, capped at `maxWorkers`). If the queue is empty and has been idle for more than `scaleDownIdle`, it removes one worker. Each worker has its own `stop` channel, so the pool can selectively shut down individual workers.

**Key insight:** The control loop pattern is a feedback system: measure (queue depth), decide (scale up/down/hold), act (add/remove workers), repeat. The scale-up is aggressive (add multiple workers per interval) because users are waiting. The scale-down is gradual (one worker per interval, with cooldown) because removing workers you will need again in seconds causes thrashing.

### Verification
```bash
go run main.go
```
Expected output (timing and exact counts vary):
```
=== Adaptive Pool with Control Loop ===
  Min workers: 2, Max workers: 15
  Scale-up threshold: queue > 10
  Scale-down idle: 3s

--- Phase 1: Burst load (50 jobs) ---
    [scale] + worker 3 (total: 3)
    [scale] + worker 4 (total: 4)
    [scale] + worker 5 (total: 5)
  [control] queue=42 workers=5 processed=3
    [scale] + worker 6 (total: 6)
    [scale] + worker 7 (total: 7)
    [scale] + worker 8 (total: 8)
  [control] queue=28 workers=8 processed=14
  [control] queue=13 workers=8 processed=27
  [control] queue=0 workers=8 processed=44
  [control] queue=0 workers=8 processed=50

--- Phase 2: Idle period ---
  [control] queue=0 workers=8 processed=50
  [control] queue=0 workers=8 processed=50
  [control] queue=0 workers=8 processed=50
  [control] queue=0 workers=8 processed=50
  [control] queue=0 workers=8 processed=50
  [control] queue=0 workers=8 processed=50
    [scale] - worker 3 (total: 7)
  [control] queue=0 workers=7 processed=50
    [scale] - worker 4 (total: 6)
  [control] queue=0 workers=6 processed=50

--- Phase 3: Second burst (30 jobs) ---
    [scale] + worker 9 (total: 7)
    [scale] + worker 10 (total: 8)
  [control] queue=22 workers=8 processed=53
  [control] queue=8 workers=8 processed=68
  [control] queue=0 workers=8 processed=78
  [control] queue=0 workers=8 processed=80

=== Final ===
  Processed: 80 jobs
```


## Step 3 -- Cooldown Logic and Metrics-Driven Decisions

Add average latency as a second scaling signal and implement proper cooldown to prevent oscillation.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	poolMinWorkers     = 2
	poolMaxWorkers     = 20
	poolQueueCapacity  = 500
	poolControlTick    = 400 * time.Millisecond
	poolScaleUpQueue   = 15
	poolLatencyTarget  = 80 * time.Millisecond
	poolCooldownPeriod = 2 * time.Second
	poolIdlePeriod     = 3 * time.Second
	poolMinJobMs       = 20
	poolMaxJobMs       = 120
)

type Metrics struct {
	processed    int64
	totalLatency int64
	windowStart  int64
	windowCount  int64
	windowNanos  int64
}

func (m *Metrics) Record(latency time.Duration) {
	atomic.AddInt64(&m.processed, 1)
	atomic.AddInt64(&m.totalLatency, int64(latency))
	atomic.AddInt64(&m.windowCount, 1)
	atomic.AddInt64(&m.windowNanos, int64(latency))
}

func (m *Metrics) WindowAvgLatency() time.Duration {
	count := atomic.LoadInt64(&m.windowCount)
	if count == 0 {
		return 0
	}
	nanos := atomic.LoadInt64(&m.windowNanos)
	return time.Duration(nanos / count)
}

func (m *Metrics) ResetWindow() {
	atomic.StoreInt64(&m.windowCount, 0)
	atomic.StoreInt64(&m.windowNanos, 0)
}

func (m *Metrics) TotalProcessed() int64 {
	return atomic.LoadInt64(&m.processed)
}

type ScaleDecision int

const (
	Hold ScaleDecision = iota
	ScaleUp
	ScaleDown
)

func (d ScaleDecision) String() string {
	switch d {
	case Hold:
		return "HOLD"
	case ScaleUp:
		return "SCALE_UP"
	case ScaleDown:
		return "SCALE_DOWN"
	default:
		return "UNKNOWN"
	}
}

type PoolWorker struct {
	ID   int
	stop chan struct{}
}

type AdaptivePool struct {
	mu             sync.Mutex
	jobs           chan int
	metrics        *Metrics
	workers        map[int]*PoolWorker
	nextID         int
	wg             sync.WaitGroup
	controlStop    chan struct{}
	lastScaleUp    time.Time
	lastScaleDown  time.Time
	lastBusyAt     time.Time
}

func NewAdaptivePool() *AdaptivePool {
	p := &AdaptivePool{
		jobs:          make(chan int, poolQueueCapacity),
		metrics:       &Metrics{},
		workers:       make(map[int]*PoolWorker),
		controlStop:   make(chan struct{}),
		lastScaleUp:   time.Now().Add(-poolCooldownPeriod),
		lastScaleDown: time.Now().Add(-poolCooldownPeriod),
		lastBusyAt:    time.Now(),
	}

	for i := 0; i < poolMinWorkers; i++ {
		p.addWorkerLocked()
	}

	go p.controlLoop()
	return p
}

func (p *AdaptivePool) addWorkerLocked() {
	if len(p.workers) >= poolMaxWorkers {
		return
	}

	p.nextID++
	w := &PoolWorker{ID: p.nextID, stop: make(chan struct{})}
	p.workers[w.ID] = w
	p.wg.Add(1)
	go p.runWorker(w)
}

func (p *AdaptivePool) removeOneLocked() {
	if len(p.workers) <= poolMinWorkers {
		return
	}
	for id, w := range p.workers {
		close(w.stop)
		delete(p.workers, id)
		return
	}
}

func (p *AdaptivePool) runWorker(w *PoolWorker) {
	defer p.wg.Done()

	for {
		select {
		case jobID, ok := <-p.jobs:
			if !ok {
				return
			}
			start := time.Now()
			dur := time.Duration(poolMinJobMs+rand.IntN(poolMaxJobMs-poolMinJobMs)) * time.Millisecond
			time.Sleep(dur)
			p.metrics.Record(time.Since(start))
			fmt.Printf("  worker %d: job %d (%v)\n", w.ID, jobID, time.Since(start).Round(time.Millisecond))
		case <-w.stop:
			return
		}
	}
}

func (p *AdaptivePool) decide(queueLen int, avgLatency time.Duration) ScaleDecision {
	now := time.Now()

	if queueLen > poolScaleUpQueue || avgLatency > poolLatencyTarget {
		if now.Sub(p.lastScaleUp) >= poolCooldownPeriod {
			return ScaleUp
		}
		return Hold
	}

	if queueLen == 0 && avgLatency == 0 {
		if now.Sub(p.lastBusyAt) > poolIdlePeriod && now.Sub(p.lastScaleDown) >= poolCooldownPeriod {
			return ScaleDown
		}
	}

	return Hold
}

func (p *AdaptivePool) controlLoop() {
	ticker := time.NewTicker(poolControlTick)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			queueLen := len(p.jobs)
			avgLatency := p.metrics.WindowAvgLatency()
			p.metrics.ResetWindow()

			p.mu.Lock()
			workerCount := len(p.workers)
			decision := p.decide(queueLen, avgLatency)

			switch decision {
			case ScaleUp:
				toAdd := queueLen / poolScaleUpQueue
				if toAdd < 1 {
					toAdd = 1
				}
				if toAdd > 4 {
					toAdd = 4
				}
				for i := 0; i < toAdd; i++ {
					p.addWorkerLocked()
				}
				p.lastScaleUp = time.Now()
				p.lastBusyAt = time.Now()
				workerCount = len(p.workers)

			case ScaleDown:
				p.removeOneLocked()
				p.lastScaleDown = time.Now()
				workerCount = len(p.workers)
			}

			p.mu.Unlock()

			if queueLen > 0 {
				p.lastBusyAt = time.Now()
			}

			latencyMs := avgLatency.Milliseconds()
			fmt.Printf("  [ctrl] q=%-3d w=%-2d lat=%-4dms decision=%s total=%d\n",
				queueLen, workerCount, latencyMs, decision, p.metrics.TotalProcessed())

		case <-p.controlStop:
			return
		}
	}
}

func (p *AdaptivePool) Submit(jobID int) {
	p.jobs <- jobID
}

func (p *AdaptivePool) Shutdown() {
	close(p.controlStop)
	close(p.jobs)
	p.wg.Wait()
}

func main() {
	pool := NewAdaptivePool()
	fmt.Println("=== Adaptive Pool with Cooldown ===")
	fmt.Printf("  Workers: %d-%d | Queue trigger: %d | Latency target: %v\n",
		poolMinWorkers, poolMaxWorkers, poolScaleUpQueue, poolLatencyTarget)
	fmt.Printf("  Cooldown: %v | Idle before scale-down: %v\n\n", poolCooldownPeriod, poolIdlePeriod)

	fmt.Println("--- Phase 1: High burst (80 jobs) ---")
	for i := 1; i <= 80; i++ {
		pool.Submit(i)
	}
	time.Sleep(4 * time.Second)

	fmt.Println()
	fmt.Println("--- Phase 2: Idle (let workers scale down) ---")
	time.Sleep(8 * time.Second)

	fmt.Println()
	fmt.Println("--- Phase 3: Moderate burst (40 jobs) ---")
	for i := 81; i <= 120; i++ {
		pool.Submit(i)
	}
	time.Sleep(4 * time.Second)

	pool.Shutdown()
	fmt.Printf("\n=== Done: %d jobs processed ===\n", pool.metrics.TotalProcessed())
}
```

**What's happening here:** The `decide` function uses two signals: queue depth (backpressure) and average latency (processing speed). Cooldown timestamps prevent scaling up or down more than once per `poolCooldownPeriod`. The metrics window resets each control tick, so latency reflects recent performance, not historical average. Scale-up is fast (up to 4 workers per tick); scale-down is slow (1 worker per tick, after idle period plus cooldown).

**Key insight:** Cooldown is essential. Without it, a fluctuating queue causes rapid add-remove cycles (thrashing) where the pool spends more time managing workers than processing jobs. The two-phase cooldown (scale-up cooldown and scale-down cooldown are independent) allows quick recovery from bursts while being conservative about removing capacity.

### Verification
```bash
go run main.go
```
Expected output (timing varies, pattern should match):
```
=== Adaptive Pool with Cooldown ===
  Workers: 2-20 | Queue trigger: 15 | Latency target: 80ms
  Cooldown: 2s | Idle before scale-down: 3s

--- Phase 1: High burst (80 jobs) ---
  [ctrl] q=76  w=6  lat=65  ms decision=SCALE_UP total=2
  [ctrl] q=64  w=10 lat=72  ms decision=HOLD total=10
  [ctrl] q=48  w=10 lat=68  ms decision=HOLD total=22
  [ctrl] q=30  w=10 lat=70  ms decision=HOLD total=38
  [ctrl] q=14  w=14 lat=74  ms decision=SCALE_UP total=50
  [ctrl] q=0   w=14 lat=66  ms decision=HOLD total=72
  [ctrl] q=0   w=14 lat=0   ms decision=HOLD total=80

--- Phase 2: Idle (let workers scale down) ---
  [ctrl] q=0   w=14 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=14 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=14 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=14 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=14 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=13 lat=0   ms decision=SCALE_DOWN total=80
  [ctrl] q=0   w=13 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=13 lat=0   ms decision=HOLD total=80
  [ctrl] q=0   w=12 lat=0   ms decision=SCALE_DOWN total=80
  ...

--- Phase 3: Moderate burst (40 jobs) ---
  [ctrl] q=36  w=6  lat=0   ms decision=SCALE_UP total=84
  [ctrl] q=22  w=8  lat=72  ms decision=HOLD total=98
  [ctrl] q=6   w=8  lat=68  ms decision=HOLD total=112
  [ctrl] q=0   w=8  lat=64  ms decision=HOLD total=120

=== Done: 120 jobs processed ===
```


## Step 4 -- Full Adaptive Pool with Load Simulation

Complete the system with a realistic variable-rate load generator that simulates production traffic patterns.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	adaptiveMinW       = 2
	adaptiveMaxW       = 25
	adaptiveQueueCap   = 1000
	adaptiveCtrlTick   = 300 * time.Millisecond
	adaptiveQueueTrig  = 20
	adaptiveLatTrig    = 100 * time.Millisecond
	adaptiveCooldown   = 1500 * time.Millisecond
	adaptiveIdleWait   = 2 * time.Second
	adaptiveMinProcMs  = 15
	adaptiveMaxProcMs  = 80
)

type AdaptiveMetrics struct {
	processed   int64
	winCount    int64
	winNanos    int64
}

func (m *AdaptiveMetrics) Record(lat time.Duration) {
	atomic.AddInt64(&m.processed, 1)
	atomic.AddInt64(&m.winCount, 1)
	atomic.AddInt64(&m.winNanos, int64(lat))
}

func (m *AdaptiveMetrics) WindowLatency() time.Duration {
	c := atomic.LoadInt64(&m.winCount)
	if c == 0 {
		return 0
	}
	return time.Duration(atomic.LoadInt64(&m.winNanos) / c)
}

func (m *AdaptiveMetrics) ResetWindow() {
	atomic.StoreInt64(&m.winCount, 0)
	atomic.StoreInt64(&m.winNanos, 0)
}

func (m *AdaptiveMetrics) Total() int64 {
	return atomic.LoadInt64(&m.processed)
}

type AWorker struct {
	id   int
	stop chan struct{}
}

type FullAdaptivePool struct {
	mu            sync.Mutex
	jobs          chan int
	metrics       *AdaptiveMetrics
	workers       map[int]*AWorker
	nextID        int
	wg            sync.WaitGroup
	ctrlStop      chan struct{}
	lastScaleUp   time.Time
	lastScaleDown time.Time
	lastBusy      time.Time
	scaleEvents   int64
}

func NewFullAdaptivePool() *FullAdaptivePool {
	p := &FullAdaptivePool{
		jobs:          make(chan int, adaptiveQueueCap),
		metrics:       &AdaptiveMetrics{},
		workers:       make(map[int]*AWorker),
		ctrlStop:      make(chan struct{}),
		lastScaleUp:   time.Now().Add(-adaptiveCooldown),
		lastScaleDown: time.Now().Add(-adaptiveCooldown),
		lastBusy:      time.Now(),
	}

	p.mu.Lock()
	for i := 0; i < adaptiveMinW; i++ {
		p.spawnLocked()
	}
	p.mu.Unlock()

	go p.controlLoop()
	return p
}

func (p *FullAdaptivePool) spawnLocked() {
	if len(p.workers) >= adaptiveMaxW {
		return
	}
	p.nextID++
	w := &AWorker{id: p.nextID, stop: make(chan struct{})}
	p.workers[w.id] = w
	p.wg.Add(1)
	go p.work(w)
}

func (p *FullAdaptivePool) removeLocked() bool {
	if len(p.workers) <= adaptiveMinW {
		return false
	}
	for id, w := range p.workers {
		close(w.stop)
		delete(p.workers, id)
		return true
	}
	return false
}

func (p *FullAdaptivePool) work(w *AWorker) {
	defer p.wg.Done()
	for {
		select {
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			start := time.Now()
			d := time.Duration(adaptiveMinProcMs+rand.IntN(adaptiveMaxProcMs-adaptiveMinProcMs)) * time.Millisecond
			time.Sleep(d)
			p.metrics.Record(time.Since(start))
			_ = j
		case <-w.stop:
			return
		}
	}
}

func (p *FullAdaptivePool) controlLoop() {
	ticker := time.NewTicker(adaptiveCtrlTick)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			q := len(p.jobs)
			lat := p.metrics.WindowLatency()
			p.metrics.ResetWindow()
			now := time.Now()

			p.mu.Lock()
			wc := len(p.workers)

			needsMore := q > adaptiveQueueTrig || (lat > adaptiveLatTrig && q > 0)
			canScaleUp := now.Sub(p.lastScaleUp) >= adaptiveCooldown

			if needsMore && canScaleUp {
				toAdd := q/adaptiveQueueTrig + 1
				if toAdd > 5 {
					toAdd = 5
				}
				for i := 0; i < toAdd; i++ {
					p.spawnLocked()
				}
				p.lastScaleUp = now
				p.lastBusy = now
				atomic.AddInt64(&p.scaleEvents, 1)
				wc = len(p.workers)
			} else if q == 0 && lat == 0 && now.Sub(p.lastBusy) > adaptiveIdleWait && now.Sub(p.lastScaleDown) >= adaptiveCooldown {
				if p.removeLocked() {
					p.lastScaleDown = now
					atomic.AddInt64(&p.scaleEvents, 1)
					wc = len(p.workers)
				}
			}

			if q > 0 {
				p.lastBusy = now
			}
			p.mu.Unlock()

			fmt.Printf("  [ctrl] q=%-4d w=%-2d lat=%-3dms done=%-4d\n",
				q, wc, lat.Milliseconds(), p.metrics.Total())

		case <-p.ctrlStop:
			return
		}
	}
}

func (p *FullAdaptivePool) Submit(jobID int) {
	p.jobs <- jobID
}

func (p *FullAdaptivePool) Shutdown() {
	close(p.ctrlStop)
	close(p.jobs)
	p.wg.Wait()
}

type LoadProfile struct {
	Rate     int
	Duration time.Duration
	Label    string
}

func generateLoad(pool *FullAdaptivePool, profiles []LoadProfile) int {
	jobID := 0
	for _, profile := range profiles {
		fmt.Printf("\n--- %s: %d jobs/sec for %v ---\n", profile.Label, profile.Rate, profile.Duration)

		if profile.Rate == 0 {
			time.Sleep(profile.Duration)
			continue
		}

		interval := time.Second / time.Duration(profile.Rate)
		deadline := time.Now().Add(profile.Duration)
		for time.Now().Before(deadline) {
			jobID++
			pool.Submit(jobID)
			time.Sleep(interval)
		}
	}
	return jobID
}

func main() {
	pool := NewFullAdaptivePool()
	fmt.Println("=== Full Adaptive Pool ===")
	fmt.Printf("  Workers: %d-%d | Cooldown: %v\n\n", adaptiveMinW, adaptiveMaxW, adaptiveCooldown)

	profiles := []LoadProfile{
		{Rate: 10, Duration: 2 * time.Second, Label: "Low traffic"},
		{Rate: 100, Duration: 3 * time.Second, Label: "Traffic spike"},
		{Rate: 0, Duration: 4 * time.Second, Label: "Quiet period"},
		{Rate: 50, Duration: 2 * time.Second, Label: "Moderate recovery"},
	}

	totalJobs := generateLoad(pool, profiles)

	time.Sleep(2 * time.Second)
	pool.Shutdown()

	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("  Total submitted: %d\n", totalJobs)
	fmt.Printf("  Total processed: %d\n", pool.metrics.Total())
	fmt.Printf("  Scale events:    %d\n", atomic.LoadInt64(&pool.scaleEvents))
}
```

**What's happening here:** The `LoadProfile` struct defines traffic patterns: rate (jobs per second) and duration. The load generator simulates real traffic with distinct phases -- low traffic, a spike, quiet, then moderate recovery. The pool responds to each phase: scaling up during the spike, holding during moderate load, and scaling down during the quiet period. The cooldown prevents oscillation between phases.

**Key insight:** This is the complete auto-scaler pattern. The three components -- metrics collection, control loop with cooldown, and worker lifecycle management -- work together exactly like a Kubernetes HPA but at the goroutine level. The key design choice is asymmetric scaling: fast up (aggressive), slow down (conservative). Every production Go service processing variable workloads benefits from this pattern.

### Verification
```bash
go run main.go
```
Expected output (timing varies, pattern matches):
```
=== Full Adaptive Pool ===
  Workers: 2-25 | Cooldown: 1.5s

--- Low traffic: 10 jobs/sec for 2s ---
  [ctrl] q=0    w=2  lat=48 ms done=3
  [ctrl] q=0    w=2  lat=52 ms done=6
  [ctrl] q=0    w=2  lat=45 ms done=9
  [ctrl] q=0    w=2  lat=50 ms done=13
  [ctrl] q=0    w=2  lat=47 ms done=17

--- Traffic spike: 100 jobs/sec for 3s ---
  [ctrl] q=22   w=4  lat=55 ms done=23
  [ctrl] q=44   w=7  lat=60 ms done=30
  [ctrl] q=38   w=10 lat=52 ms done=56
  [ctrl] q=25   w=13 lat=48 ms done=90
  [ctrl] q=10   w=13 lat=45 ms done=135
  [ctrl] q=0    w=13 lat=42 ms done=180

--- Quiet period: 0 jobs/sec for 4s ---
  [ctrl] q=0    w=13 lat=0  ms done=220
  [ctrl] q=0    w=13 lat=0  ms done=220
  ...
  [ctrl] q=0    w=12 lat=0  ms done=220
  [ctrl] q=0    w=11 lat=0  ms done=220

--- Moderate recovery: 50 jobs/sec for 2s ---
  [ctrl] q=12   w=11 lat=50 ms done=230
  [ctrl] q=5    w=11 lat=48 ms done=260
  [ctrl] q=0    w=11 lat=45 ms done=290

=== Summary ===
  Total submitted: 320
  Total processed: 320
  Scale events:    8
```


## Common Mistakes

### Scaling Without Cooldown (Thrashing)

```go
package main

import (
	"fmt"
)

func main() {
	workers := 5
	for i := 0; i < 10; i++ {
		queueLen := []int{20, 0, 25, 0, 18, 0, 22, 0, 15, 0}[i]
		if queueLen > 10 {
			workers += 3
			fmt.Printf("  tick %d: queue=%d -> scale UP to %d workers\n", i, queueLen, workers)
		} else if queueLen == 0 {
			workers -= 2
			if workers < 2 {
				workers = 2
			}
			fmt.Printf("  tick %d: queue=%d -> scale DOWN to %d workers\n", i, queueLen, workers)
		}
	}
	fmt.Println("  Result: constant scaling up and down (thrashing)")
}
```
**What happens:** Without cooldown, every tick that sees a high queue adds workers, and every tick that sees an empty queue removes them. The pool oscillates wildly, spending CPU on goroutine management instead of work.

**Fix:** Track `lastScaleUp` and `lastScaleDown` timestamps. Only allow scaling if enough time has passed since the last scale event. This dampens oscillation.


### Closing the Jobs Channel Before Workers Finish

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	jobs := make(chan int, 10)

	go func() {
		for j := range jobs {
			time.Sleep(50 * time.Millisecond)
			fmt.Printf("  processed job %d\n", j)
		}
	}()

	for i := 1; i <= 5; i++ {
		jobs <- i
	}

	close(jobs)
	// main exits immediately -- worker goroutine may not finish all jobs
	fmt.Println("done")
}
```
**What happens:** Closing the channel is correct for signaling "no more jobs." But if `main` exits before the worker goroutine finishes processing all buffered jobs, some jobs are silently dropped.

**Fix:** Use `sync.WaitGroup` to wait for all workers to drain:
```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	jobs := make(chan int, 10)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := range jobs {
			time.Sleep(50 * time.Millisecond)
			fmt.Printf("  processed job %d\n", j)
		}
	}()

	for i := 1; i <= 5; i++ {
		jobs <- i
	}

	close(jobs)
	wg.Wait() // blocks until worker processes all remaining jobs
	fmt.Println("done")
}
```


## Review

Dynamic scaling is a feedback loop -- measure, decide, act, repeat -- and the
whole design lives in how it decides. Queue depth is the backpressure signal
and average latency is the processing-speed signal; a window that resets each
tick keeps both reflecting recent load rather than a diluted lifetime average.
The action is deliberately asymmetric: scale up several workers at once because
users are queued and waiting, but scale down one worker per tick and only after
an idle period, because capacity you shed now is capacity you may need in
seconds. Independent scale-up and scale-down cooldown timestamps are what stop
the pool from oscillating -- thrashing between add and remove -- when load
flickers between ticks. Each worker carries its own stop channel so the control
loop can retire one goroutine at a time without disturbing the rest, and atomic
counters let the control goroutine read metrics the workers write without a
lock. The result mirrors a Kubernetes HPA, only at goroutine granularity.

To confirm the pieces are really independent, extend the pool into a
`PriorityAdaptivePool`: two buffered queues, high and low, where workers drain
the high queue first via a `select` with `default` before falling back to the
low queue. The control loop should scale on the combined depth of both queues
while still favoring the high queue, and print the ratio of high-to-low jobs
processed each tick. Drive it with a steady stream of low-priority work
interrupted by a burst of high-priority work and confirm the burst jumps ahead
even while the pool is busy -- all while every worker stays individually
stoppable and both queues drain cleanly on shutdown.

## Resources
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) -- monitor the active goroutine count to confirm the pool actually shrinks.
- [time.Ticker](https://pkg.go.dev/time#Ticker) -- the periodic timer that paces the control loop.
- [sync/atomic](https://pkg.go.dev/sync/atomic) -- lock-free counters the workers write and the control loop reads.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- the goroutine and channel patterns the pool is built from.

---

Back to [Concurrency](../../concurrency.md) | Next: [24-goroutine-per-connection-tcp](../24-goroutine-per-connection-tcp/24-goroutine-per-connection-tcp.md)
