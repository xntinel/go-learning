# Exercise 29: Goroutine Work Stealing

A batch processor hands N tasks to W workers. If every task takes the same time, round-robin distribution is fine. Real workloads never do -- one task parses a 10-row CSV in a millisecond, the next chews through a 100,000-row file for half a second -- so a worker that happens to draw several long tasks finishes last while its neighbors sit idle, and the slowest worker alone sets the wall-clock time. Work-stealing removes that imbalance: each worker owns a local queue, and when it drains, it steals from a busier peer. This exercise builds the stealable deque and a full scheduler, then measures it head-to-head against static distribution on an identical task set.

## What you'll build

```text
goroutine-work-stealing/
  main.go        a static round-robin baseline with imbalance metrics, a
                 thread-safe stealable deque (owner Pop, thief Steal), and a
                 full work-stealing scheduler compared against the baseline
```

- Build: two schedulers over the same variable-duration task set -- static round-robin and work-stealing -- with wall-time, imbalance, and efficiency reported for each.
- Implement: `staticDistribute` and `runStaticWorker`; a `StealableQueue` with `Pop` (owner, LIFO) and `Steal` (thief, FIFO); and `runWorkStealingExperiment` with randomized victim selection and atomic termination detection.
- Verify: `go run main.go` at each step, comparing static wall time against work-stealing wall time.

### Why the slowest worker sets the wall time

Work-stealing solves this: each worker has a local queue of tasks. When a worker finishes its queue, it "steals" tasks from another worker's queue. Fast-finishing workers migrate to help slow-finishing workers, naturally balancing the load. This is the same principle Go's own scheduler uses: when a P (processor) has no goroutines in its local run queue, it steals from another P's queue.

The pattern appears in every system that processes variable-duration work: database query executors, HTTP request handlers with mixed endpoints, CI/CD pipelines with variable build times, and MapReduce implementations. Understanding work-stealing at the application level gives insight into why Go's scheduler is effective and when you need to implement similar balancing yourself.


## Step 1 -- Static Distribution Baseline

Implement a basic round-robin task distributor and measure completion time. Tasks have highly variable durations to highlight the imbalance problem.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	totalTasks = 40
	numWorkers = 4
	minTaskMs  = 5
	maxTaskMs  = 100
)

type Task struct {
	ID       int
	Duration time.Duration
}

type WorkerStats struct {
	ID         int
	TasksDone  int
	TotalWork  time.Duration
	IdleAfter  time.Duration
}

func generateTasks(rng *rand.Rand) []Task {
	tasks := make([]Task, totalTasks)
	for i := range tasks {
		ms := minTaskMs + rng.IntN(maxTaskMs-minTaskMs+1)
		tasks[i] = Task{ID: i, Duration: time.Duration(ms) * time.Millisecond}
	}
	return tasks
}

func staticDistribute(tasks []Task, workers int) [][]Task {
	queues := make([][]Task, workers)
	for i := range queues {
		queues[i] = make([]Task, 0, len(tasks)/workers+1)
	}
	for i, task := range tasks {
		w := i % workers
		queues[w] = append(queues[w], task)
	}
	return queues
}

func runStaticWorker(id int, tasks []Task, start time.Time, wg *sync.WaitGroup, stats chan<- WorkerStats) {
	defer wg.Done()
	var totalWork time.Duration

	for _, task := range tasks {
		time.Sleep(task.Duration)
		totalWork += task.Duration
	}

	stats <- WorkerStats{
		ID:        id,
		TasksDone: len(tasks),
		TotalWork: totalWork,
		IdleAfter: time.Since(start) - totalWork,
	}
}

func main() {
	rng := rand.New(rand.NewPCG(42, 0))
	tasks := generateTasks(rng)

	var totalDuration time.Duration
	for _, t := range tasks {
		totalDuration += t.Duration
	}
	fmt.Printf("=== Static Distribution ===\n")
	fmt.Printf("  Tasks: %d | Workers: %d | Total work: %v\n",
		totalTasks, numWorkers, totalDuration.Round(time.Millisecond))
	fmt.Printf("  Ideal time (perfect balance): %v\n\n",
		(totalDuration / time.Duration(numWorkers)).Round(time.Millisecond))

	queues := staticDistribute(tasks, numWorkers)

	statsCh := make(chan WorkerStats, numWorkers)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go runStaticWorker(i, queues[i], start, &wg, statsCh)
	}

	wg.Wait()
	elapsed := time.Since(start)
	close(statsCh)

	fmt.Printf("  %-10s %-10s %-15s\n", "Worker", "Tasks", "Work Time")
	fmt.Println("  ------------------------------------")

	var maxWork, minWork time.Duration
	minWork = time.Hour
	for stats := range statsCh {
		fmt.Printf("  %-10d %-10d %-15v\n",
			stats.ID, stats.TasksDone, stats.TotalWork.Round(time.Millisecond))
		if stats.TotalWork > maxWork {
			maxWork = stats.TotalWork
		}
		if stats.TotalWork < minWork {
			minWork = stats.TotalWork
		}
	}

	imbalance := maxWork - minWork
	fmt.Printf("\n  Wall time: %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Imbalance (max - min work): %v\n", imbalance.Round(time.Millisecond))
	fmt.Printf("  Efficiency: %.1f%%\n", float64(totalDuration)/float64(elapsed*time.Duration(numWorkers))*100)
}
```

**What's happening here:** Forty tasks with durations between 5ms and 100ms are distributed round-robin across 4 workers. Each worker processes its assigned queue sequentially. Because tasks have variable durations, some workers finish much earlier than others. The imbalance metric shows the difference between the busiest and least busy worker.

**Key insight:** With random task durations, round-robin distribution produces significant imbalance. The worker that happens to receive several long tasks determines the total wall time. Workers that finish early sit idle. The efficiency metric shows what fraction of total worker-time was spent doing actual work versus idling.

### Verification
```bash
go run main.go
```
Expected output (the task set and per-worker work times are deterministic with seed 42; wall time and efficiency vary slightly by run):
```
=== Static Distribution ===
  Tasks: 40 | Workers: 4 | Total work: 2.206s
  Ideal time (perfect balance): 552ms

  Worker     Tasks      Work Time
  ------------------------------------
  2          10         495ms
  0          10         537ms
  3          10         547ms
  1          10         627ms

  Wall time: 633ms
  Imbalance (max - min work): 132ms
  Efficiency: 87.2%
```


## Step 2 -- Work-Stealing Queues

Implement a thread-safe deque (double-ended queue) that supports owner-side pop from the bottom and thief-side steal from the top. This is the data structure at the heart of work-stealing.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Task struct {
	ID       int
	Duration time.Duration
}

type StealableQueue struct {
	mu    sync.Mutex
	tasks []Task
}

func NewStealableQueue(tasks []Task) *StealableQueue {
	copied := make([]Task, len(tasks))
	copy(copied, tasks)
	return &StealableQueue{tasks: copied}
}

func (q *StealableQueue) Pop() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return Task{}, false
	}
	task := q.tasks[len(q.tasks)-1]
	q.tasks = q.tasks[:len(q.tasks)-1]
	return task, true
}

func (q *StealableQueue) Steal() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return Task{}, false
	}
	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	return task, true
}

func (q *StealableQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

func main() {
	tasks := []Task{
		{ID: 0, Duration: 10 * time.Millisecond},
		{ID: 1, Duration: 20 * time.Millisecond},
		{ID: 2, Duration: 30 * time.Millisecond},
		{ID: 3, Duration: 40 * time.Millisecond},
		{ID: 4, Duration: 50 * time.Millisecond},
	}

	q := NewStealableQueue(tasks)
	fmt.Printf("Queue size: %d\n\n", q.Len())

	fmt.Println("Owner pops from bottom (LIFO):")
	if task, ok := q.Pop(); ok {
		fmt.Printf("  Popped: Task %d (%v)\n", task.ID, task.Duration)
	}
	if task, ok := q.Pop(); ok {
		fmt.Printf("  Popped: Task %d (%v)\n", task.ID, task.Duration)
	}

	fmt.Printf("\nQueue size after 2 pops: %d\n\n", q.Len())

	fmt.Println("Thief steals from top (FIFO):")
	if task, ok := q.Steal(); ok {
		fmt.Printf("  Stole: Task %d (%v)\n", task.ID, task.Duration)
	}

	fmt.Printf("\nQueue size after steal: %d\n", q.Len())

	fmt.Println()
	fmt.Println("Remaining tasks:")
	for {
		task, ok := q.Pop()
		if !ok {
			break
		}
		fmt.Printf("  Task %d (%v)\n", task.ID, task.Duration)
	}
}
```

**What's happening here:** The `StealableQueue` is a double-ended queue. The owner worker pops from the bottom (LIFO -- most recently added tasks first). A thief worker steals from the top (FIFO -- oldest tasks first). This asymmetry is intentional: the owner gets warm-cache tasks (recently added), while the thief gets tasks that have been waiting longest (improving fairness).

**Key insight:** In production work-stealing implementations (like Go's scheduler), the deque uses lock-free atomic operations for the owner side and a mutex only for the thief side. Our simplified version uses a mutex for both sides, which is correct but slower under high contention. The conceptual model is what matters: owner pops locally without contention most of the time; stealing is the rare, expensive operation.

### Verification
```bash
go run main.go
```
Expected output:
```
Queue size: 5

Owner pops from bottom (LIFO):
  Popped: Task 4 (50ms)
  Popped: Task 3 (40ms)

Queue size after 2 pops: 3

Thief steals from top (FIFO):
  Stole: Task 0 (10ms)

Queue size after steal: 2

Remaining tasks:
  Task 2 (30ms)
  Task 1 (20ms)
```


## Step 3 -- Work-Stealing Scheduler

Combine the stealable queues with a full work-stealing scheduler. Compare it directly against static distribution on the same task set.

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
	totalTasks = 40
	numWorkers = 4
	minTaskMs  = 5
	maxTaskMs  = 100
)

type Task struct {
	ID       int
	Duration time.Duration
}

type StealableQueue struct {
	mu    sync.Mutex
	tasks []Task
}

func NewStealableQueue(tasks []Task) *StealableQueue {
	copied := make([]Task, len(tasks))
	copy(copied, tasks)
	return &StealableQueue{tasks: copied}
}

func (q *StealableQueue) Pop() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return Task{}, false
	}
	task := q.tasks[len(q.tasks)-1]
	q.tasks = q.tasks[:len(q.tasks)-1]
	return task, true
}

func (q *StealableQueue) Steal() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return Task{}, false
	}
	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	return task, true
}

func (q *StealableQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

type WorkerReport struct {
	ID        int
	Processed int
	Stolen    int
	WorkTime  time.Duration
}

func generateTasks(rng *rand.Rand) []Task {
	tasks := make([]Task, totalTasks)
	for i := range tasks {
		ms := minTaskMs + rng.IntN(maxTaskMs-minTaskMs+1)
		tasks[i] = Task{ID: i, Duration: time.Duration(ms) * time.Millisecond}
	}
	return tasks
}

func staticDistribute(tasks []Task, workers int) [][]Task {
	queues := make([][]Task, workers)
	for i := range queues {
		queues[i] = make([]Task, 0, len(tasks)/workers+1)
	}
	for i, task := range tasks {
		queues[i%workers] = append(queues[i%workers], task)
	}
	return queues
}

func runStaticExperiment(tasks []Task) (time.Duration, []WorkerReport) {
	queues := staticDistribute(tasks, numWorkers)
	reports := make(chan WorkerReport, numWorkers)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int, myTasks []Task) {
			defer wg.Done()
			var workTime time.Duration
			for _, task := range myTasks {
				time.Sleep(task.Duration)
				workTime += task.Duration
			}
			reports <- WorkerReport{ID: id, Processed: len(myTasks), WorkTime: workTime}
		}(i, queues[i])
	}

	wg.Wait()
	elapsed := time.Since(start)
	close(reports)

	var results []WorkerReport
	for r := range reports {
		results = append(results, r)
	}
	return elapsed, results
}

func runWorkStealingExperiment(tasks []Task) (time.Duration, []WorkerReport) {
	queues := staticDistribute(tasks, numWorkers)
	stealQueues := make([]*StealableQueue, numWorkers)
	for i := range stealQueues {
		stealQueues[i] = NewStealableQueue(queues[i])
	}

	reports := make(chan WorkerReport, numWorkers)
	var wg sync.WaitGroup
	var activeWorkers int32
	atomic.StoreInt32(&activeWorkers, int32(numWorkers))

	start := time.Now()
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var processed, stolen int
			var workTime time.Duration
			myQueue := stealQueues[id]
			rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()+int64(id)), 0))

			for {
				if task, ok := myQueue.Pop(); ok {
					time.Sleep(task.Duration)
					workTime += task.Duration
					processed++
					continue
				}

				found := false
				attempts := 0
				for attempts < numWorkers*2 {
					victim := rng.IntN(numWorkers)
					if victim == id {
						continue
					}
					if task, ok := stealQueues[victim].Steal(); ok {
						time.Sleep(task.Duration)
						workTime += task.Duration
						processed++
						stolen++
						found = true
						break
					}
					attempts++
				}

				if !found {
					current := atomic.AddInt32(&activeWorkers, -1)
					if current <= 0 {
						reports <- WorkerReport{ID: id, Processed: processed, Stolen: stolen, WorkTime: workTime}
						return
					}
					time.Sleep(1 * time.Millisecond)
					atomic.AddInt32(&activeWorkers, 1)
				}
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)
	close(reports)

	var results []WorkerReport
	for r := range reports {
		results = append(results, r)
	}
	return elapsed, results
}

func main() {
	rng := rand.New(rand.NewPCG(42, 0))
	tasks := generateTasks(rng)

	var totalDuration time.Duration
	for _, t := range tasks {
		totalDuration += t.Duration
	}

	fmt.Printf("=== Work Stealing vs Static Distribution ===\n")
	fmt.Printf("  Tasks: %d | Workers: %d | Total work: %v\n",
		totalTasks, numWorkers, totalDuration.Round(time.Millisecond))
	fmt.Printf("  Theoretical minimum: %v\n\n",
		(totalDuration / time.Duration(numWorkers)).Round(time.Millisecond))

	staticTime, staticReports := runStaticExperiment(tasks)
	fmt.Println("--- Static Distribution ---")
	fmt.Printf("  %-8s %-8s %-12s\n", "Worker", "Tasks", "Work Time")
	for _, r := range staticReports {
		fmt.Printf("  %-8d %-8d %-12v\n", r.ID, r.Processed, r.WorkTime.Round(time.Millisecond))
	}
	fmt.Printf("  Wall time: %v\n\n", staticTime.Round(time.Millisecond))

	stealTime, stealReports := runWorkStealingExperiment(tasks)
	fmt.Println("--- Work Stealing ---")
	fmt.Printf("  %-8s %-8s %-8s %-12s\n", "Worker", "Tasks", "Stolen", "Work Time")
	for _, r := range stealReports {
		fmt.Printf("  %-8d %-8d %-8d %-12v\n", r.ID, r.Processed, r.Stolen, r.WorkTime.Round(time.Millisecond))
	}
	fmt.Printf("  Wall time: %v\n\n", stealTime.Round(time.Millisecond))

	improvement := float64(staticTime-stealTime) / float64(staticTime) * 100
	fmt.Println("=== Comparison ===")
	fmt.Printf("  Static:        %v\n", staticTime.Round(time.Millisecond))
	fmt.Printf("  Work stealing: %v\n", stealTime.Round(time.Millisecond))
	fmt.Printf("  Improvement:   %.1f%% faster\n", improvement)
	fmt.Printf("  Static efficiency:  %.1f%%\n", float64(totalDuration)/float64(staticTime*time.Duration(numWorkers))*100)
	fmt.Printf("  Stealing efficiency: %.1f%%\n", float64(totalDuration)/float64(stealTime*time.Duration(numWorkers))*100)
}
```

**What's happening here:** Both experiments use the same tasks (deterministic seed). The static experiment distributes tasks round-robin and each worker processes its fixed queue. The work-stealing experiment starts the same way, but when a worker's queue is empty, it randomly picks another worker and tries to steal from their queue. The termination condition uses an atomic counter: when a worker finds no tasks locally and cannot steal, it decrements the counter and pauses briefly. If all workers have decremented, the work is complete.

**Key insight:** Work-stealing achieves better wall time because fast-finishing workers help slow-finishing workers. The "stolen" column shows the rebalancing in action. The efficiency metric approaches 100% because idle time is minimized. The 1ms sleep when a steal attempt fails prevents busy-spinning while allowing quick recovery when new tasks become available (e.g., a victim worker might still be running and has not yet moved to the "empty" state).

### Verification
```bash
go run main.go
```
Expected output (the task set is deterministic with seed 42, so the static half is stable; the work-stealing table, wall times, and improvement vary by run because each worker seeds its own RNG from the clock):
```
=== Work Stealing vs Static Distribution ===
  Tasks: 40 | Workers: 4 | Total work: 2.206s
  Theoretical minimum: 552ms

--- Static Distribution ---
  Worker   Tasks    Work Time
  2        10       495ms
  0        10       537ms
  3        10       547ms
  1        10       627ms
  Wall time: 633ms

--- Work Stealing ---
  Worker   Tasks    Stolen   Work Time
  2        12       2        605ms
  3        9        0        534ms
  1        9        0        530ms
  0        10       0        537ms
  Wall time: 613ms

=== Comparison ===
  Static:        633ms
  Work stealing: 613ms
  Improvement:   3.1% faster
  Static efficiency:  87.1%
  Stealing efficiency: 89.9%
```


## Common Mistakes

### Stealing Without Randomization (Always Steal from Worker 0)

```go
// Wrong: always try to steal from the same worker
func stealTask(id int, queues []*StealableQueue) (Task, bool) {
	for victim := 0; victim < len(queues); victim++ {
		if victim == id {
			continue
		}
		if task, ok := queues[victim].Steal(); ok {
			return task, true
		}
	}
	return Task{}, false
}
```
**What happens:** All idle workers converge on worker 0's queue. They contend on worker 0's mutex, creating a bottleneck. Worker 1's queue might have tasks but nobody checks it first. Under high contention, the stealing overhead can exceed the benefit.

**Fix:** Randomize the starting victim. Each thief picks a random worker to steal from. This distributes contention across all queues and avoids the thundering herd on a single victim.


### Busy-Spinning When No Work Is Available

```go
// Wrong: spin loop when no tasks available
for {
	task, ok := myQueue.Pop()
	if !ok {
		// try stealing
		task, ok = stealFrom(otherQueues)
		if !ok {
			continue // burns CPU spinning, checking empty queues millions of times
		}
	}
	process(task)
}
```
**What happens:** When all queues are empty (work is almost done), idle workers spin at full CPU checking empty queues. This wastes CPU and can actually slow down the last remaining worker due to cache contention.

**Fix:** Add a small sleep (1-5ms) between failed steal attempts, or use a condition variable to wake up thieves only when new work appears.


### Not Terminating Workers Correctly

```go
// Wrong: worker exits when its queue is empty, ignoring potential steals
func worker(id int, queue *StealableQueue, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		task, ok := queue.Pop()
		if !ok {
			return // exits immediately -- doesn't try stealing
		}
		process(task)
	}
}
```
**What happens:** Workers exit as soon as their local queue is empty. No stealing occurs. The result is identical to static distribution, defeating the purpose of work-stealing entirely.

**Fix:** When the local queue is empty, attempt stealing from other workers. Only terminate when all queues across all workers are empty and no workers are currently processing tasks.


## Review

The whole gain comes from turning a static assignment into a dynamic one. Round-robin fixes each worker's queue up front, so when task durations vary the busiest worker alone determines the wall time while the others idle -- the efficiency metric quantifies exactly how much worker-time is wasted. Work-stealing lets an idle worker pull tasks from a busier peer's queue, and the deque's asymmetry is what makes that cheap and fair: the owner pops from its own end (LIFO, keeping recently added, cache-warm tasks local) while thieves take from the far end (FIFO, grabbing the oldest waiting work). Randomized victim selection keeps every thief from converging on one queue and contending on a single mutex, and termination detection -- the atomic active-worker count -- is the subtle part, because a worker must tell "my queue is momentarily empty" apart from "all work everywhere is done." The trade is a little overhead (mutex contention and failed steal attempts) for a much tighter load balance, and it is precisely the mechanism Go's own runtime uses when an idle P steals from a busy one.

To be sure the idea is yours, extend the scheduler with task splitting: when a worker steals a task whose estimated duration exceeds 50ms, split it into two half-length sub-tasks, process one, and push the other back onto its own queue where it can be stolen again. Compare three strategies on the same forty tasks -- static, basic work-stealing, and work-stealing with splitting -- reporting wall time and effective parallelism (total work over wall time times workers) for each. Mark sub-tasks as non-splittable so a task splits only once, and you will see splitting cut the tail that a single oversized task would otherwise leave.

## Resources

- [Go Scheduler Design](https://go.dev/src/runtime/proc.go) -- the runtime source where Go implements the same work-stealing over per-P run queues.
- [Work-Stealing Paper (Blumofe & Leiserson)](https://dl.acm.org/doi/10.1145/324133.324234) -- the foundational analysis proving work-stealing's time and space bounds.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) -- the mutual exclusion guarding the shared deque in this simplified version.

---

Back to [Concurrency](../../concurrency.md) | Next: [30-goroutine-dag-orchestrator](../30-goroutine-dag-orchestrator/30-goroutine-dag-orchestrator.md)
