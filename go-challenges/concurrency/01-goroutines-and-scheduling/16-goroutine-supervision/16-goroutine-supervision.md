# Exercise 16: Goroutine Supervision

Production services lean on background goroutines that are supposed to outlive every request and never stop -- a metrics collector, a health reporter, a cache warmer. But a goroutine that panics dies silently: the metrics stop flowing, the health endpoint starts lying, the cache goes cold, and nobody notices until a customer reports degraded performance. Go ships no built-in supervision, so this exercise builds it, growing from a single recoverable worker to a multi-worker supervisor that recovers panics, restarts under a configurable budget, reports every crash through a channel, and gives up loudly when a worker is beyond saving.

## What you'll build

```text
goroutine-supervision/
  main.go        single recoverable worker, then a Supervisor managing three
                 workers, a flaky worker with auto-restart, and permanent
                 failure once the restart budget is exhausted
```

- Build: a supervisor that launches long-lived workers, catches their panics, and restarts them under a bounded retry policy.
- Implement: a `Worker` with `RunWithRecovery` / `runUntilPanic`; a `Supervisor` with `superviseWorker` and `runWorker`; a `SupervisorConfig` carrying `MaxRestarts` and `RestartDelay`; and a `CrashReport` routed over a channel.
- Verify: `go run main.go` at each step (panic timing is randomized, so runs differ).

### Why a panicking goroutine dies silently

Production microservices run background goroutines that must stay alive for the lifetime of the process: a metrics collector that scrapes system stats every 10 seconds, a health reporter that pings dependencies, a cache warmer that pre-loads hot keys. These are not request-scoped goroutines that start and stop -- they are long-lived workers that the service depends on.

The problem is that goroutines can panic. A nil pointer dereference in the metrics collector, an unexpected API response in the health reporter, a malformed cache key -- any of these crashes the goroutine. Without supervision, the goroutine dies silently. The metrics stop flowing, the health endpoint starts lying, the cache goes cold. Nobody notices until a customer reports degraded performance.

The supervisor pattern solves this: a manager goroutine launches workers, monitors them for panics, and restarts them automatically with a configurable delay. If a worker exceeds its maximum restart count, the supervisor marks it as permanently failed and alerts the operator. This pattern is borrowed from Erlang's OTP supervisor -- adapted here to Go's goroutine model.

The key challenge is that Go does not provide built-in supervision. You build it yourself with `defer`/`recover`, channels, and careful state management. This exercise constructs the full pattern from a single recoverable worker to a multi-worker supervisor with crash reporting and restart limits.


## Step 1 -- Single Worker with Recovery

A worker function runs in a loop, performing periodic work. When it panics, `defer`/`recover` catches the panic, reports the crash, and the wrapping function restarts the worker. A restart counter tracks how many times recovery has occurred.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	workerInterval = 200 * time.Millisecond
	maxRestarts    = 3
	restartDelay   = 100 * time.Millisecond
)

type CrashReport struct {
	WorkerName string
	Reason     string
	RestartNum int
	Timestamp  time.Time
}

type Worker struct {
	Name       string
	Task       func(iteration int)
	restarts   int
}

func NewWorker(name string, task func(iteration int)) *Worker {
	return &Worker{Name: name, Task: task}
}

func (w *Worker) RunWithRecovery(crashes chan<- CrashReport, done chan<- string) {
	iteration := 0
	for w.restarts <= maxRestarts {
		stopped := w.runUntilPanic(iteration, crashes)
		if !stopped {
			done <- w.Name
			return
		}
		w.restarts++
		if w.restarts > maxRestarts {
			break
		}
		fmt.Printf("  [SUPERVISOR] Restarting %s in %v (restart %d/%d)\n",
			w.Name, restartDelay, w.restarts, maxRestarts)
		time.Sleep(restartDelay)
		iteration = 0
	}
	done <- w.Name
}

func (w *Worker) runUntilPanic(startIteration int, crashes chan<- CrashReport) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			crashes <- CrashReport{
				WorkerName: w.Name,
				Reason:     fmt.Sprintf("%v", r),
				RestartNum: w.restarts + 1,
				Timestamp:  time.Now(),
			}
			panicked = true
		}
	}()

	for i := startIteration; i < 10; i++ {
		w.Task(i)
		time.Sleep(workerInterval)
	}
	return false
}

func main() {
	crashes := make(chan CrashReport, 10)
	done := make(chan string, 1)

	metricsTask := func(iteration int) {
		if rand.IntN(5) == 0 && iteration > 0 {
			panic("nil pointer: metric endpoint returned nil response")
		}
		fmt.Printf("  [metrics-collector] tick %d: collected %d metrics\n",
			iteration, rand.IntN(50)+10)
	}

	worker := NewWorker("metrics-collector", metricsTask)

	fmt.Println("=== Single Worker with Recovery ===")
	fmt.Println()
	go worker.RunWithRecovery(crashes, done)

	go func() {
		for crash := range crashes {
			fmt.Printf("  [CRASH] %s panicked: %s (restart #%d at %s)\n",
				crash.WorkerName, crash.Reason, crash.RestartNum,
				crash.Timestamp.Format("15:04:05.000"))
		}
	}()

	finishedWorker := <-done
	fmt.Printf("\n  Worker %q finished (restarts used: %d/%d)\n",
		finishedWorker, worker.restarts, maxRestarts)

	if worker.restarts > maxRestarts {
		fmt.Printf("  STATUS: PERMANENTLY FAILED -- exceeded max restarts\n")
	} else {
		fmt.Printf("  STATUS: COMPLETED NORMALLY\n")
	}
}
```

**What's happening here:** The `RunWithRecovery` method wraps the actual work loop (`runUntilPanic`) in a restart loop. Each time `runUntilPanic` panics, the deferred recover catches it, sends a `CrashReport` through the channel, and returns `true`. The outer loop increments the restart counter and, if under the limit, sleeps briefly before restarting. If the worker completes all iterations without panicking, it returns `false` and exits cleanly.

**Key insight:** The `runUntilPanic` function uses a named return value (`panicked bool`) that the deferred function modifies. This is a clean pattern for communicating panic status without additional channels. The panic value is captured as a string in the `CrashReport`, giving operators exactly what went wrong.

### Verification
```bash
go run main.go
```
Expected output (panics are random; this shows a typical run):
```
=== Single Worker with Recovery ===

  [metrics-collector] tick 0: collected 34 metrics
  [metrics-collector] tick 1: collected 22 metrics
  [CRASH] metrics-collector panicked: nil pointer: metric endpoint returned nil response (restart #1 at 10:15:02.345)
  [SUPERVISOR] Restarting metrics-collector in 100ms (restart 1/3)
  [metrics-collector] tick 0: collected 45 metrics
  [metrics-collector] tick 1: collected 18 metrics
  [metrics-collector] tick 2: collected 31 metrics
  ...

  Worker "metrics-collector" finished (restarts used: 1/3)
  STATUS: COMPLETED NORMALLY
```


## Step 2 -- Supervisor Managing Three Workers

Build a `Supervisor` struct that manages multiple named workers. Each worker represents a different microservice background task. The supervisor launches all workers, monitors crashes, and prints a status dashboard when all workers finish.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	workerTicks  = 8
	tickInterval = 100 * time.Millisecond
	maxRestarts  = 3
	restartDelay = 50 * time.Millisecond
)

type CrashReport struct {
	WorkerName string
	Reason     string
	RestartNum int
	Timestamp  time.Time
}

type WorkerStatus struct {
	Name       string
	Restarts   int
	Completed  bool
	Failed     bool
}

type WorkerFunc func(workerName string, iteration int)

type Supervisor struct {
	workers  map[string]WorkerFunc
	statuses map[string]*WorkerStatus
	crashes  chan CrashReport
	mu       sync.Mutex
}

func NewSupervisor() *Supervisor {
	return &Supervisor{
		workers:  make(map[string]WorkerFunc),
		statuses: make(map[string]*WorkerStatus),
		crashes:  make(chan CrashReport, 50),
	}
}

func (s *Supervisor) Register(name string, fn WorkerFunc) {
	s.workers[name] = fn
	s.statuses[name] = &WorkerStatus{Name: name}
}

func (s *Supervisor) StartAll() {
	var wg sync.WaitGroup

	for name, fn := range s.workers {
		wg.Add(1)
		go s.superviseWorker(name, fn, &wg)
	}

	go func() {
		wg.Wait()
		close(s.crashes)
	}()

	for crash := range s.crashes {
		fmt.Printf("  [CRASH] %-20s %s (restart #%d)\n",
			crash.WorkerName, crash.Reason, crash.RestartNum)
	}
}

func (s *Supervisor) superviseWorker(name string, fn WorkerFunc, wg *sync.WaitGroup) {
	defer wg.Done()
	restarts := 0

	for {
		panicked := s.runWorker(name, fn)
		if !panicked {
			s.markCompleted(name, restarts)
			return
		}

		restarts++
		if restarts > maxRestarts {
			s.markFailed(name, restarts-1)
			return
		}

		s.updateRestarts(name, restarts)
		time.Sleep(restartDelay)
	}
}

func (s *Supervisor) runWorker(name string, fn WorkerFunc) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			s.crashes <- CrashReport{
				WorkerName: name,
				Reason:     fmt.Sprintf("%v", r),
				RestartNum: s.getRestarts(name) + 1,
				Timestamp:  time.Now(),
			}
			panicked = true
		}
	}()

	for i := 0; i < workerTicks; i++ {
		fn(name, i)
		time.Sleep(tickInterval)
	}
	return false
}

func (s *Supervisor) markCompleted(name string, restarts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[name].Completed = true
	s.statuses[name].Restarts = restarts
}

func (s *Supervisor) markFailed(name string, restarts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[name].Failed = true
	s.statuses[name].Restarts = restarts
}

func (s *Supervisor) updateRestarts(name string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[name].Restarts = count
}

func (s *Supervisor) getRestarts(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[name].Restarts
}

func (s *Supervisor) PrintStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Println()
	fmt.Println("  === Supervisor Status ===")
	fmt.Println("  Worker               Restarts  Status")
	fmt.Println("  ──────               ────────  ──────")
	for _, ws := range s.statuses {
		status := "UNKNOWN"
		if ws.Completed {
			status = "COMPLETED"
		} else if ws.Failed {
			status = "PERMANENTLY FAILED"
		}
		fmt.Printf("  %-20s %5d     %s\n", ws.Name, ws.Restarts, status)
	}
}

func main() {
	sup := NewSupervisor()

	sup.Register("metrics-collector", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: scraped %d metrics\n", name, i, rand.IntN(40)+10)
	})

	sup.Register("health-reporter", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: all dependencies healthy\n", name, i)
	})

	sup.Register("cache-warmer", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: warmed %d keys\n", name, i, rand.IntN(100)+50)
	})

	fmt.Println("=== Supervisor Managing 3 Workers ===")
	fmt.Println()
	sup.StartAll()
	sup.PrintStatus()
}
```

**What's happening here:** The `Supervisor` registers three workers, each with a distinct task function. `StartAll` launches one supervision goroutine per worker. Each supervision goroutine runs the worker in a loop: if it panics, check the restart budget; if it completes normally, mark it as done. The crash channel centralizes all crash reports for logging. After all workers finish (via WaitGroup), the supervisor prints a status table.

**Key insight:** The supervisor goroutine per worker is the control plane. The worker function is the data plane. Separating them means the supervisor's restart logic never panics (it only calls `recover`), while the worker code can be arbitrarily complex and crash-prone. This separation of concerns is what makes the pattern robust.

### Verification
```bash
go run main.go
```
Expected output (no panics since these workers are stable):
```
=== Supervisor Managing 3 Workers ===

  [metrics-collector] tick 0: scraped 34 metrics
  [health-reporter] tick 0: all dependencies healthy
  [cache-warmer] tick 0: warmed 87 keys
  [metrics-collector] tick 1: scraped 22 metrics
  [health-reporter] tick 1: all dependencies healthy
  [cache-warmer] tick 1: warmed 112 keys
  ...
  [cache-warmer] tick 7: warmed 95 keys
  [metrics-collector] tick 7: scraped 45 metrics
  [health-reporter] tick 7: all dependencies healthy

  === Supervisor Status ===
  Worker               Restarts  Status
  ──────               ────────  ──────
  metrics-collector        0     COMPLETED
  health-reporter          0     COMPLETED
  cache-warmer             0     COMPLETED
```


## Step 3 -- Flaky Worker with Auto-Restart

Introduce a worker that panics randomly. The supervisor catches each panic, logs it, waits the configured delay, and restarts the worker. The worker resumes from iteration 0 on each restart.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	workerTicks  = 8
	tickInterval = 80 * time.Millisecond
	maxRestarts  = 5
	restartDelay = 100 * time.Millisecond
	panicChance  = 0.25
)

type CrashReport struct {
	WorkerName string
	Reason     string
	RestartNum int
	Timestamp  time.Time
}

type WorkerStatus struct {
	Name      string
	Restarts  int
	Completed bool
	Failed    bool
}

type SupervisorConfig struct {
	MaxRestarts  int
	RestartDelay time.Duration
}

type WorkerFunc func(workerName string, iteration int)

type Supervisor struct {
	config   SupervisorConfig
	workers  map[string]WorkerFunc
	statuses map[string]*WorkerStatus
	crashes  []CrashReport
	crashCh  chan CrashReport
	mu       sync.Mutex
}

func NewSupervisor(config SupervisorConfig) *Supervisor {
	return &Supervisor{
		config:   config,
		workers:  make(map[string]WorkerFunc),
		statuses: make(map[string]*WorkerStatus),
		crashCh:  make(chan CrashReport, 50),
	}
}

func (s *Supervisor) Register(name string, fn WorkerFunc) {
	s.workers[name] = fn
	s.statuses[name] = &WorkerStatus{Name: name}
}

func (s *Supervisor) StartAll() {
	var wg sync.WaitGroup

	for name, fn := range s.workers {
		wg.Add(1)
		go s.superviseWorker(name, fn, &wg)
	}

	go func() {
		wg.Wait()
		close(s.crashCh)
	}()

	for crash := range s.crashCh {
		s.mu.Lock()
		s.crashes = append(s.crashes, crash)
		s.mu.Unlock()
		fmt.Printf("  [CRASH] %-20s panic: %s (restart #%d at %s)\n",
			crash.WorkerName, crash.Reason, crash.RestartNum,
			crash.Timestamp.Format("15:04:05.000"))
	}
}

func (s *Supervisor) superviseWorker(name string, fn WorkerFunc, wg *sync.WaitGroup) {
	defer wg.Done()
	restarts := 0

	for {
		panicked := s.runWorker(name, fn)
		if !panicked {
			s.setStatus(name, restarts, true, false)
			return
		}

		restarts++
		if restarts > s.config.MaxRestarts {
			s.setStatus(name, restarts-1, false, true)
			fmt.Printf("  [SUPERVISOR] %s PERMANENTLY FAILED after %d restarts\n",
				name, restarts-1)
			return
		}

		fmt.Printf("  [SUPERVISOR] Restarting %s in %v (%d/%d)\n",
			name, s.config.RestartDelay, restarts, s.config.MaxRestarts)
		s.setStatus(name, restarts, false, false)
		time.Sleep(s.config.RestartDelay)
	}
}

func (s *Supervisor) runWorker(name string, fn WorkerFunc) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			s.crashCh <- CrashReport{
				WorkerName: name,
				Reason:     fmt.Sprintf("%v", r),
				RestartNum: s.getRestartCount(name) + 1,
				Timestamp:  time.Now(),
			}
			panicked = true
		}
	}()

	for i := 0; i < workerTicks; i++ {
		fn(name, i)
		time.Sleep(tickInterval)
	}
	return false
}

func (s *Supervisor) setStatus(name string, restarts int, completed, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.statuses[name]
	ws.Restarts = restarts
	ws.Completed = completed
	ws.Failed = failed
}

func (s *Supervisor) getRestartCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[name].Restarts
}

func (s *Supervisor) PrintReport() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Println()
	fmt.Println("  === Supervisor Final Report ===")
	fmt.Println("  Worker               Restarts  Status")
	fmt.Println("  ──────               ────────  ──────")
	for _, ws := range s.statuses {
		status := "RUNNING"
		if ws.Completed {
			status = "COMPLETED"
		} else if ws.Failed {
			status = "PERM. FAILED"
		}
		fmt.Printf("  %-20s %5d     %s\n", ws.Name, ws.Restarts, status)
	}

	fmt.Printf("\n  Total crashes recorded: %d\n", len(s.crashes))
	if len(s.crashes) > 0 {
		fmt.Println()
		fmt.Println("  --- Crash Log ---")
		for _, c := range s.crashes {
			fmt.Printf("  %s  %-20s  restart #%d  %s\n",
				c.Timestamp.Format("15:04:05.000"), c.WorkerName,
				c.RestartNum, c.Reason)
		}
	}
}

func main() {
	config := SupervisorConfig{
		MaxRestarts:  maxRestarts,
		RestartDelay: restartDelay,
	}
	sup := NewSupervisor(config)

	sup.Register("metrics-collector", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: scraped %d metrics\n", name, i, rand.IntN(40)+10)
	})

	sup.Register("health-reporter", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: deps healthy\n", name, i)
	})

	sup.Register("cache-warmer", func(name string, i int) {
		if rand.Float64() < panicChance {
			keys := []string{"user:1001", "product:nil", "session:expired"}
			badKey := keys[rand.IntN(len(keys))]
			panic(fmt.Sprintf("nil dereference processing key %q", badKey))
		}
		fmt.Printf("  [%s] tick %d: warmed %d keys\n", name, i, rand.IntN(100)+50)
	})

	fmt.Println("=== Supervisor with Flaky Worker ===")
	fmt.Println()
	sup.StartAll()
	sup.PrintReport()
}
```

**What's happening here:** The `cache-warmer` has a 25% chance of panicking on each tick. The supervisor catches each panic, logs it as a crash report, increments the restart counter, waits 100ms, and relaunches. The stable `metrics-collector` and `health-reporter` finish their 8 ticks without incident. After all workers complete (or permanently fail), the supervisor prints a full crash log.

**Key insight:** The `SupervisorConfig` struct makes retry behavior configurable per deployment. A development environment might set `MaxRestarts: 0` to fail fast and surface bugs. Production might set `MaxRestarts: 10` with exponential backoff. The crash channel decouples detection (inside `runWorker`) from reporting (in `StartAll`), so you could route crashes to a monitoring system, a log aggregator, or an alerting service.

### Verification
```bash
go run main.go
```
Expected output (panics are random; typical run shown):
```
=== Supervisor with Flaky Worker ===

  [metrics-collector] tick 0: scraped 28 metrics
  [health-reporter] tick 0: deps healthy
  [cache-warmer] tick 0: warmed 87 keys
  [cache-warmer] tick 1: warmed 112 keys
  [CRASH] cache-warmer          panic: nil dereference processing key "product:nil" (restart #1 at 10:20:03.456)
  [SUPERVISOR] Restarting cache-warmer in 100ms (1/5)
  [metrics-collector] tick 2: scraped 35 metrics
  [health-reporter] tick 2: deps healthy
  [cache-warmer] tick 0: warmed 94 keys
  [cache-warmer] tick 1: warmed 73 keys
  [cache-warmer] tick 2: warmed 55 keys
  ...

  === Supervisor Final Report ===
  Worker               Restarts  Status
  ──────               ────────  ──────
  metrics-collector        0     COMPLETED
  health-reporter          0     COMPLETED
  cache-warmer             2     COMPLETED

  Total crashes recorded: 2

  --- Crash Log ---
  10:20:03.456  cache-warmer          restart #1  nil dereference processing key "product:nil"
  10:20:04.123  cache-warmer          restart #2  nil dereference processing key "session:expired"
```


## Step 4 -- Permanent Failure on Max Restarts Exceeded

Make the flaky worker so unstable that it exhausts its restart budget. The supervisor detects this, marks the worker as permanently failed, and continues running the healthy workers. The final report distinguishes completed workers from failed ones.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

const (
	workerTicks  = 10
	tickInterval = 60 * time.Millisecond
	maxRestarts  = 3
	restartDelay = 80 * time.Millisecond
)

type CrashReport struct {
	WorkerName string
	Reason     string
	RestartNum int
	Timestamp  time.Time
}

type WorkerStatus struct {
	Name      string
	Restarts  int
	Completed bool
	Failed    bool
}

type SupervisorConfig struct {
	MaxRestarts  int
	RestartDelay time.Duration
}

type WorkerFunc func(workerName string, iteration int)

type Supervisor struct {
	config   SupervisorConfig
	workers  []string
	funcs    map[string]WorkerFunc
	statuses map[string]*WorkerStatus
	crashes  []CrashReport
	crashCh  chan CrashReport
	mu       sync.Mutex
}

func NewSupervisor(config SupervisorConfig) *Supervisor {
	return &Supervisor{
		config:   config,
		funcs:    make(map[string]WorkerFunc),
		statuses: make(map[string]*WorkerStatus),
		crashCh:  make(chan CrashReport, 50),
	}
}

func (s *Supervisor) Register(name string, fn WorkerFunc) {
	s.workers = append(s.workers, name)
	s.funcs[name] = fn
	s.statuses[name] = &WorkerStatus{Name: name}
}

func (s *Supervisor) StartAll() {
	var wg sync.WaitGroup

	for _, name := range s.workers {
		wg.Add(1)
		go s.superviseWorker(name, s.funcs[name], &wg)
	}

	go func() {
		wg.Wait()
		close(s.crashCh)
	}()

	for crash := range s.crashCh {
		s.mu.Lock()
		s.crashes = append(s.crashes, crash)
		s.mu.Unlock()
		fmt.Printf("  [CRASH] %-20s %s (attempt #%d at %s)\n",
			crash.WorkerName, crash.Reason, crash.RestartNum,
			crash.Timestamp.Format("15:04:05.000"))
	}
}

func (s *Supervisor) superviseWorker(name string, fn WorkerFunc, wg *sync.WaitGroup) {
	defer wg.Done()
	restarts := 0

	for {
		panicked := s.runWorker(name, fn)
		if !panicked {
			s.setStatus(name, restarts, true, false)
			fmt.Printf("  [SUPERVISOR] %s completed normally\n", name)
			return
		}

		restarts++
		if restarts > s.config.MaxRestarts {
			s.setStatus(name, s.config.MaxRestarts, false, true)
			fmt.Printf("  [SUPERVISOR] %s PERMANENTLY FAILED (exhausted %d/%d restarts)\n",
				name, s.config.MaxRestarts, s.config.MaxRestarts)
			return
		}

		fmt.Printf("  [SUPERVISOR] Restarting %s (%d/%d) after %v delay\n",
			name, restarts, s.config.MaxRestarts, s.config.RestartDelay)
		s.setStatus(name, restarts, false, false)
		time.Sleep(s.config.RestartDelay)
	}
}

func (s *Supervisor) runWorker(name string, fn WorkerFunc) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			s.crashCh <- CrashReport{
				WorkerName: name,
				Reason:     fmt.Sprintf("%v", r),
				RestartNum: s.getRestarts(name) + 1,
				Timestamp:  time.Now(),
			}
			panicked = true
		}
	}()

	for i := 0; i < workerTicks; i++ {
		fn(name, i)
		time.Sleep(tickInterval)
	}
	return false
}

func (s *Supervisor) setStatus(name string, restarts int, completed, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.statuses[name]
	ws.Restarts = restarts
	ws.Completed = completed
	ws.Failed = failed
}

func (s *Supervisor) getRestarts(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[name].Restarts
}

func (s *Supervisor) PrintFinalReport() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════════╗")
	fmt.Println("  ║           SUPERVISOR FINAL REPORT                ║")
	fmt.Println("  ╠══════════════════════════════════════════════════╣")

	completed := 0
	failed := 0

	sortedNames := make([]string, len(s.workers))
	copy(sortedNames, s.workers)
	slices.Sort(sortedNames)

	for _, name := range sortedNames {
		ws := s.statuses[name]
		status := "RUNNING"
		marker := "  "
		if ws.Completed {
			status = "OK"
			marker = "  "
			completed++
		} else if ws.Failed {
			status = "DEAD"
			marker = "!!"
			failed++
		}
		fmt.Printf("  ║ %s %-20s restarts: %d  status: %-6s  ║\n",
			marker, ws.Name, ws.Restarts, status)
	}

	fmt.Println("  ╠══════════════════════════════════════════════════╣")
	fmt.Printf("  ║  Completed: %d | Failed: %d | Total: %-14d║\n",
		completed, failed, completed+failed)
	fmt.Printf("  ║  Total crashes: %-34d║\n", len(s.crashes))
	fmt.Println("  ╚══════════════════════════════════════════════════╝")

	if len(s.crashes) > 0 {
		fmt.Println()
		fmt.Println("  --- Full Crash History ---")
		for i, c := range s.crashes {
			fmt.Printf("  %2d. %s  %-20s  attempt #%d  %s\n",
				i+1, c.Timestamp.Format("15:04:05.000"),
				c.WorkerName, c.RestartNum, c.Reason)
		}
	}

	if failed > 0 {
		fmt.Println()
		fmt.Println("  ACTION REQUIRED: Some workers are permanently failed.")
		fmt.Println("  Investigate crash reasons above and deploy a fix.")
	}
}

func main() {
	config := SupervisorConfig{
		MaxRestarts:  maxRestarts,
		RestartDelay: restartDelay,
	}
	sup := NewSupervisor(config)

	sup.Register("metrics-collector", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: scraped %d datapoints\n",
			name, i, rand.IntN(40)+10)
	})

	sup.Register("health-reporter", func(name string, i int) {
		fmt.Printf("  [%s] tick %d: all systems green\n", name, i)
	})

	// This worker panics almost every tick -- will exhaust restarts
	sup.Register("cache-warmer", func(name string, i int) {
		if rand.Float64() < 0.80 {
			errors := []string{
				"nil pointer on key lookup",
				"index out of range [5] with length 3",
				"invalid memory address or nil pointer dereference",
				"runtime error: slice bounds out of range",
			}
			panic(errors[rand.IntN(len(errors))])
		}
		fmt.Printf("  [%s] tick %d: warmed %d keys\n",
			name, i, rand.IntN(100)+50)
	})

	fmt.Println("=== Goroutine Supervision: Permanent Failure ===")
	fmt.Println()
	start := time.Now()
	sup.StartAll()
	elapsed := time.Since(start)

	sup.PrintFinalReport()
	fmt.Printf("\n  Total runtime: %v\n", elapsed.Round(time.Millisecond))
}
```

**What's happening here:** The `cache-warmer` has an 80% panic rate -- it almost always crashes on every tick. With `MaxRestarts: 3`, it panics 4 times (initial run + 3 restarts) and is marked as permanently failed. The supervisor continues running `metrics-collector` and `health-reporter` to completion. The final report shows exactly which worker died, how many crashes occurred, and the full crash history with timestamps.

**Key insight:** The supervisor never crashes itself. Each `runWorker` call is isolated behind `defer`/`recover`. The outer `superviseWorker` loop is pure control flow -- no panicking code runs there. This means a catastrophically broken worker cannot take down the entire service. In production, the "ACTION REQUIRED" message would trigger an alert in your monitoring system.

**Design note:** The ordered `workers` slice preserves registration order while `funcs` and `statuses` maps provide O(1) lookup. This is a common Go pattern when you need both order and fast access.

### Verification
```bash
go run main.go
```
Expected output (cache-warmer almost certainly exhausts restarts):
```
=== Goroutine Supervision: Permanent Failure ===

  [metrics-collector] tick 0: scraped 28 datapoints
  [health-reporter] tick 0: all systems green
  [CRASH] cache-warmer          nil pointer on key lookup (attempt #1 at 10:30:01.234)
  [SUPERVISOR] Restarting cache-warmer (1/3) after 80ms delay
  [metrics-collector] tick 1: scraped 35 datapoints
  [health-reporter] tick 1: all systems green
  [CRASH] cache-warmer          index out of range [5] with length 3 (attempt #2 at 10:30:01.456)
  [SUPERVISOR] Restarting cache-warmer (2/3) after 80ms delay
  [CRASH] cache-warmer          runtime error: slice bounds out of range (attempt #3 at 10:30:01.600)
  [SUPERVISOR] Restarting cache-warmer (3/3) after 80ms delay
  [CRASH] cache-warmer          nil pointer on key lookup (attempt #4 at 10:30:01.750)
  [SUPERVISOR] cache-warmer PERMANENTLY FAILED (exhausted 3/3 restarts)
  ...
  [metrics-collector] tick 9: scraped 41 datapoints
  [health-reporter] tick 9: all systems green
  [SUPERVISOR] metrics-collector completed normally
  [SUPERVISOR] health-reporter completed normally

  ╔══════════════════════════════════════════════════╗
  ║           SUPERVISOR FINAL REPORT                ║
  ╠══════════════════════════════════════════════════╣
  ║    cache-warmer           restarts: 3  status: DEAD    ║
  ║    health-reporter        restarts: 0  status: OK      ║
  ║    metrics-collector      restarts: 0  status: OK      ║
  ╠══════════════════════════════════════════════════╣
  ║  Completed: 2 | Failed: 1 | Total: 3              ║
  ║  Total crashes: 4                                  ║
  ╚══════════════════════════════════════════════════╝

  --- Full Crash History ---
   1. 10:30:01.234  cache-warmer          attempt #1  nil pointer on key lookup
   2. 10:30:01.456  cache-warmer          attempt #2  index out of range [5] with length 3
   3. 10:30:01.600  cache-warmer          attempt #3  runtime error: slice bounds out of range
   4. 10:30:01.750  cache-warmer          attempt #4  nil pointer on key lookup

  ACTION REQUIRED: Some workers are permanently failed.
  Investigate crash reasons above and deploy a fix.

  Total runtime: 680ms
```


## Common Mistakes

### Placing recover() in the Wrong Goroutine

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"time"
)

func worker() {
	panic("worker crashed")
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("recovered:", r) // NEVER REACHED for goroutine panics
		}
	}()

	go worker()
	time.Sleep(time.Second)
	fmt.Println("main continues") // program crashes before reaching this
}
```
**What happens:** `recover()` in main cannot catch a panic in a different goroutine. The program crashes with an unrecovered panic.

**Correct -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

func worker(wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("recovered in worker:", r)
		}
	}()
	panic("worker crashed")
}

func main() {
	var wg sync.WaitGroup
	wg.Add(1)
	go worker(&wg)
	wg.Wait()
	fmt.Println("main continues safely")
}
```

### Not Signaling WaitGroup on Panic

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

func worker(id int, wg *sync.WaitGroup) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("worker %d recovered: %v\n", id, r)
			// wg.Done() NOT called -- WaitGroup counter stuck
		}
	}()
	defer wg.Done()
	if id == 1 {
		panic("bad data")
	}
	fmt.Printf("worker %d done\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go worker(i, &wg)
	}
	wg.Wait() // hangs -- wait, does it? Check the defer order
	fmt.Println("done")
}
```
**What happens:** This example is actually subtly correct because `wg.Done()` is deferred before the recover function -- LIFO means `recover` runs first, then `Done`. But if you swap the defer order (put `Done` after `recover`), the program deadlocks. Always put `defer wg.Done()` as the FIRST defer.

**Correct -- explicit ordering:**
```go
package main

import (
	"fmt"
	"sync"
)

func worker(id int, wg *sync.WaitGroup) {
	defer wg.Done() // FIRST defer = runs LAST = always executes
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("worker %d recovered: %v\n", id, r)
		}
	}()
	if id == 1 {
		panic("bad data")
	}
	fmt.Printf("worker %d done\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go worker(i, &wg)
	}
	wg.Wait()
	fmt.Println("done")
}
```

### Restarting Without a Limit

**Wrong -- complete program:**
```go
package main

import "fmt"

func unstableWorker() {
	panic("always crashes")
}

func supervise() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Println("restarting after:", r)
				}
			}()
			unstableWorker()
		}()
		// no restart limit -- infinite loop burning CPU
	}
}

func main() {
	supervise() // runs forever, printing "restarting after: always crashes" in a tight loop
}
```
**What happens:** A permanently broken worker creates an infinite restart loop that burns CPU and floods logs. In production, this can cause cascading failures as the restart loop consumes resources needed by healthy workers.

**Correct -- with a restart budget:**
```go
package main

import (
	"fmt"
	"time"
)

const maxRestarts = 3

func unstableWorker() {
	panic("always crashes")
}

func supervise() {
	restarts := 0
	for restarts <= maxRestarts {
		func() {
			defer func() {
				if r := recover(); r != nil {
					restarts++
					fmt.Printf("crash #%d: %v\n", restarts, r)
				}
			}()
			unstableWorker()
		}()
		if restarts > maxRestarts {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("Worker permanently failed after %d restarts\n", maxRestarts)
}

func main() {
	supervise()
}
```


## Review

The pattern rests on a strict separation of planes. The supervisor goroutine is the control plane: it launches workers, waits on their outcome, and applies restart policy, but it never runs user code directly, which is what makes it crash-proof by design. The worker is the data plane, arbitrarily complex and allowed to panic. The bridge between them is `defer`/`recover`, and its placement is non-negotiable -- a `recover` only catches a panic in its own goroutine, so it must sit inside the worker, never in the manager. A named return value in the deferred function is how `runWorker` reports "I panicked" back to the loop without a second channel, and `defer wg.Done()` must be the first defer (LIFO means it runs last) so cleanup happens on every path, panic or not. Around that core, `SupervisorConfig` bounds the damage: `MaxRestarts` stops an unfixable worker from looping forever and burning CPU, and the `CrashReport` channel decouples detection from reporting so crashes can be logged, aggregated, or alerted on -- and a permanent failure becomes an operator alert rather than silent degradation.

To prove you can build it unaided, assemble a "service mesh monitor" that manages four workers -- `log-shipper`, `cert-rotator`, `config-syncer`, `rate-limiter` -- each running fifteen 50ms ticks, with `cert-rotator` panicking about 30% of the time and `rate-limiter` about 50%, under a config of `MaxRestarts: 4` and `RestartDelay: 150ms`. When every worker has finished or been declared dead, print a report with each worker's final status and restart count, the full crash history sorted by timestamp, and an uptime figure per worker computed from how many ticks actually executed. If you can track ticks completed across restarts and reason about why the flaky pair burns its budget while the stable pair never restarts, the pattern is yours.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) -- how the three interact, and why recover only works inside a deferred function.
- [Go Spec: Handling Panics](https://go.dev/ref/spec#Handling_panics) -- the exact rule that a recover catches only panics in its own goroutine.
- [Erlang OTP Supervisor](https://www.erlang.org/doc/design_principles/sup_princ) -- the supervision-tree design this pattern is borrowed from.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- the counter the supervisor uses to wait for every worker to terminate.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) -- what guards the shared status map that the supervisor reads and writes.

---

Back to [Concurrency](../../concurrency.md) | Next: [17-concurrent-map-reduce](../17-concurrent-map-reduce/17-concurrent-map-reduce.md)
