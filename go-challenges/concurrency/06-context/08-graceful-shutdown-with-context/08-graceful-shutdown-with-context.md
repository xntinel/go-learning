# Exercise 8: Graceful Shutdown with Context

When a container orchestrator sends SIGTERM -- a Kubernetes rolling update, an
ECS deployment, a dyno restart -- your service has a narrow window to shut down
cleanly: stop taking new work, let in-flight requests finish, and close
connections and flush buffers before it exits. Skip that and clients see dropped
502s, database transactions stay open with locks held, and buffered log entries
vanish; in a payments system a debit can commit while its credit never does. The
Go answer is to catch the signal, cancel a root context, and let cancellation
propagate down the context tree to every goroutine while a WaitGroup holds the
exit until they finish. This exercise builds that sequence and shows what breaks
without it.

## What you'll build

```text
08-graceful-shutdown-with-context/
  main.go        signal-driven shutdown: NotifyContext, context-tree
                 cancellation, WaitGroup drain with timeout, resource cleanup
```

- Build: a production graceful-shutdown sequence for a multi-worker service.
- Implement: `signal.NotifyContext` signal handling, a `RequestProcessor` that drains in-flight requests with a WaitGroup and timeout, worker loops that watch `ctx.Done()`, a `WaitForWorkers` with a deadline, and post-drain resource cleanup.
- Verify: `go run main.go`

### Why cancellation must reach every goroutine

When a container orchestrator sends SIGTERM (Kubernetes rolling update, ECS deployment, Heroku dyno restart), your service has a limited window to shut down cleanly. During this window it must:

1. Stop accepting new work
2. Signal all running workers to finish their current task
3. Wait for workers to complete (up to a deadline)
4. Close database connections, flush log buffers, close file handles
5. Exit with a clean status

Without graceful shutdown, in-flight HTTP requests get dropped (clients see 502), database transactions are left open (locks held indefinitely), and log entries are lost (buffered data never flushed). In a banking system, this can mean a debit was processed but the credit was not. In an analytics pipeline, this means data loss.

The Go pattern is elegant: catch the OS signal, cancel a root context, and let the context tree propagate the shutdown signal to every goroutine in the system. A `sync.WaitGroup` ensures main waits for all workers to finish before exiting. A shutdown timeout prevents hanging on stuck workers.

## Step 1 -- signal.NotifyContext: The Modern Signal Handler

Go 1.16 introduced `signal.NotifyContext`, which creates a context that is automatically cancelled when the process receives a signal. This replaces the older pattern of manually creating channels and goroutines:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	autoShutdownDelay = 2 * time.Second
	cleanupDelay      = 100 * time.Millisecond
)

func simulateSignal(delay time.Duration) {
	time.Sleep(delay)
	fmt.Println("[auto] triggering shutdown (simulating SIGTERM)")
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGTERM)
}

func runCleanup() {
	fmt.Println("[server] starting cleanup...")
	time.Sleep(cleanupDelay)
	fmt.Println("[server] cleanup complete, exiting")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go simulateSignal(autoShutdownDelay)

	fmt.Println("[server] running... (press Ctrl+C or wait 2s)")
	fmt.Printf("[server] PID: %d\n", os.Getpid())

	<-ctx.Done()
	stop()

	fmt.Printf("[server] shutdown signal received: %v\n", ctx.Err())
	runCleanup()
}
```

### Verification
```bash
go run main.go
```
Press Ctrl+C or wait 2 seconds. Expected output:
```
[server] running... (press Ctrl+C or wait 2s)
[server] PID: 12345
[auto] triggering shutdown (simulating SIGTERM)
[server] shutdown signal received: context canceled
[server] starting cleanup...
[server] cleanup complete, exiting
```

`signal.NotifyContext` is cleaner than the manual channel approach: no goroutine management, no buffered channel sizing. Calling `stop()` after receiving the signal restores default signal handling, so a second Ctrl+C force-kills the process (safety valve).

## Step 2 -- Draining In-Flight Requests

When shutdown starts, the server must stop accepting new requests and wait for in-flight requests to complete. Build a request processor with a WaitGroup to track active requests:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const drainTimeout = 2 * time.Second

type RequestProcessor struct {
	wg        sync.WaitGroup
	accepting bool
}

func NewRequestProcessor() *RequestProcessor {
	return &RequestProcessor{accepting: true}
}

func (rp *RequestProcessor) HandleRequest(ctx context.Context, reqID string) {
	rp.wg.Add(1)
	go func() {
		defer rp.wg.Done()
		duration := time.Duration(100+len(reqID)*20) * time.Millisecond

		fmt.Printf("[req %s] started (will take %v)\n", reqID, duration)

		select {
		case <-time.After(duration):
			fmt.Printf("[req %s] completed\n", reqID)
		case <-ctx.Done():
			fmt.Printf("[req %s] cancelled: %v\n", reqID, ctx.Err())
		}
	}()
}

func (rp *RequestProcessor) DrainAndWait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		rp.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (rp *RequestProcessor) StopAccepting() {
	rp.accepting = false
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	processor := NewRequestProcessor()

	processor.HandleRequest(ctx, "order-1")
	processor.HandleRequest(ctx, "order-22")
	processor.HandleRequest(ctx, "order-333")

	time.Sleep(50 * time.Millisecond)

	fmt.Println("\n[system] SIGTERM received, stopping new requests")
	processor.StopAccepting()

	fmt.Println("[system] rejecting new request: order-4444 (shutting down)")

	fmt.Printf("[system] draining in-flight requests (timeout: %v)...\n", drainTimeout)
	cancel()

	if processor.DrainAndWait(drainTimeout) {
		fmt.Println("[system] all requests drained successfully")
	} else {
		fmt.Println("[system] drain timeout exceeded, some requests may be lost")
	}
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
[req order-1] started (will take 220ms)
[req order-22] started (will take 240ms)
[req order-333] started (will take 260ms)

[system] SIGTERM received, stopping new requests
[system] rejecting new request: order-4444 (shutting down)
[system] draining in-flight requests (timeout: 2s)...
[req order-1] cancelled: context canceled
[req order-22] cancelled: context canceled
[req order-333] cancelled: context canceled
[system] all requests drained successfully
```

The WaitGroup tracks every in-flight request. `DrainAndWait` runs `wg.Wait()` in a goroutine so it can be combined with a timeout -- preventing the server from hanging indefinitely if a request is stuck.

## Step 3 -- Complete Server Shutdown Sequence

Build the full production pattern: receive signal, drain requests, close database connections, flush log buffers, exit:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	serverRunDuration = 1 * time.Second
	shutdownTimeout   = 5 * time.Second
	httpTickInterval  = 200 * time.Millisecond
	queueTickInterval = 300 * time.Millisecond
	httpDrainDelay    = 150 * time.Millisecond
	queueCommitDelay  = 100 * time.Millisecond
	dbCloseDelay      = 50 * time.Millisecond
	logFlushDelay     = 30 * time.Millisecond
)

type Server struct {
	wg sync.WaitGroup
}

func NewServer() *Server {
	return &Server{}
}

func (s *Server) StartHTTPWorker(ctx context.Context, name string) {
	defer s.wg.Done()
	fmt.Printf("[%s] listening\n", name)

	ticker := time.NewTicker(httpTickInterval)
	defer ticker.Stop()

	count := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("[%s] draining %d in-flight requests...\n", name, count%3)
			time.Sleep(httpDrainDelay)
			fmt.Printf("[%s] stopped\n", name)
			return
		case <-ticker.C:
			count++
			fmt.Printf("[%s] handled request #%d\n", name, count)
		}
	}
}

func (s *Server) StartQueueConsumer(ctx context.Context, name string) {
	defer s.wg.Done()
	fmt.Printf("[%s] polling queue\n", name)

	ticker := time.NewTicker(queueTickInterval)
	defer ticker.Stop()

	count := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("[%s] finishing current message...\n", name)
			time.Sleep(queueCommitDelay)
			fmt.Printf("[%s] committed offset, stopped\n", name)
			return
		case <-ticker.C:
			count++
			fmt.Printf("[%s] processed message #%d\n", name, count)
		}
	}
}

func (s *Server) WaitForWorkers(timeout time.Duration) bool {
	workersDone := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(workersDone)
	}()

	select {
	case <-workersDone:
		return true
	case <-time.After(timeout):
		return false
	}
}

func closeDatabase() {
	fmt.Println("[db] closing connection pool...")
	time.Sleep(dbCloseDelay)
	fmt.Println("[db] all connections returned to pool and closed")
}

func flushLogs() {
	fmt.Println("[logs] flushing buffered log entries...")
	time.Sleep(logFlushDelay)
	fmt.Println("[logs] 247 entries flushed to disk")
}

func main() {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	server := NewServer()

	server.wg.Add(3)
	go server.StartHTTPWorker(rootCtx, "http-8080")
	go server.StartHTTPWorker(rootCtx, "http-8443")
	go server.StartQueueConsumer(rootCtx, "queue-orders")

	time.Sleep(serverRunDuration)

	fmt.Println("\n========================================")
	fmt.Println("[system] SIGTERM received")
	fmt.Println("[system] initiating graceful shutdown...")
	fmt.Printf("========================================\n\n")

	rootCancel()

	fmt.Printf("[system] waiting up to %v for workers to drain...\n\n", shutdownTimeout)

	if server.WaitForWorkers(shutdownTimeout) {
		fmt.Println("\n[system] all workers stopped gracefully")
	} else {
		fmt.Println("\n[system] WARNING: shutdown timeout exceeded, forcing exit")
	}

	fmt.Println()
	closeDatabase()
	flushLogs()

	fmt.Println("\n[system] shutdown complete")
}
```

### Verification
```bash
go run main.go
```
Expected output (abbreviated):
```
[http-8080] listening
[http-8443] listening
[queue-orders] polling queue
[http-8080] handled request #1
[http-8443] handled request #1
[queue-orders] processed message #1
...

========================================
[system] SIGTERM received
[system] initiating graceful shutdown...
========================================

[system] waiting up to 5s for workers to drain...

[http-8080] draining 2 in-flight requests...
[http-8443] draining 2 in-flight requests...
[queue-orders] finishing current message...
[queue-orders] committed offset, stopped
[http-8080] stopped
[http-8443] stopped

[system] all workers stopped gracefully

[db] closing connection pool...
[db] all connections returned to pool and closed
[logs] flushing buffered log entries...
[logs] 247 entries flushed to disk

[system] shutdown complete
```

The shutdown sequence: (1) cancel root context to signal all workers, (2) wait for workers to drain with a timeout, (3) close database connections, (4) flush logs. Each phase completes before the next starts.

## Step 4 -- What Happens Without Graceful Shutdown

This demonstrates the consequences of calling `os.Exit` or not waiting for workers:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const hardKillTimeout = 120 * time.Millisecond

type PaymentProcessor struct{}

func NewPaymentProcessor() *PaymentProcessor {
	return &PaymentProcessor{}
}

func (p *PaymentProcessor) Process(ctx context.Context, wg *sync.WaitGroup, id string) {
	defer wg.Done()
	fmt.Printf("[payment] %s: debit started\n", id)
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("[payment] %s: debit completed, starting credit...\n", id)

	select {
	case <-time.After(200 * time.Millisecond):
		fmt.Printf("[payment] %s: credit completed (CONSISTENT)\n", id)
	case <-ctx.Done():
		fmt.Printf("[payment] %s: CREDIT NEVER COMPLETED (INCONSISTENT STATE!)\n", id)
	}
}

func writeTransactionLog(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Println("[db] writing transaction log...")
	select {
	case <-time.After(150 * time.Millisecond):
		fmt.Println("[db] transaction log committed")
	case <-ctx.Done():
		fmt.Println("[db] TRANSACTION LOG LOST (data corruption risk)")
	}
}

func flushMetrics(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	fmt.Println("[metrics] flushing 1,247 data points to Prometheus...")
	select {
	case <-time.After(100 * time.Millisecond):
		fmt.Println("[metrics] flush complete")
	case <-ctx.Done():
		fmt.Println("[metrics] METRICS LOST (gap in monitoring dashboards)")
	}
}

func printConsequences() {
	fmt.Println("\n=== Consequences ===")
	fmt.Println("1. Payment PAY-001: money debited but never credited")
	fmt.Println("2. Transaction log: lost, no audit trail")
	fmt.Println("3. Metrics: 1,247 data points gone, dashboards show gaps")
	fmt.Println("\nThis is why graceful shutdown matters.")
}

func main() {
	fmt.Printf("=== Without Graceful Shutdown (hard kill after %v) ===\n\n", hardKillTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), hardKillTimeout)
	defer cancel()

	var wg sync.WaitGroup
	processor := NewPaymentProcessor()

	wg.Add(3)
	go processor.Process(ctx, &wg, "PAY-001")
	go writeTransactionLog(ctx, &wg)
	go flushMetrics(ctx, &wg)

	wg.Wait()
	printConsequences()
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Without Graceful Shutdown (hard kill after 120ms) ===

[payment] PAY-001: debit started
[db] writing transaction log...
[metrics] flushing 1,247 data points to Prometheus...
[payment] PAY-001: debit completed, starting credit...
[metrics] METRICS LOST (gap in monitoring dashboards)
[db] TRANSACTION LOG LOST (data corruption risk)
[payment] PAY-001: CREDIT NEVER COMPLETED (INCONSISTENT STATE!)

=== Consequences ===
1. Payment PAY-001: money debited but never credited
2. Transaction log: lost, no audit trail
3. Metrics: 1,247 data points gone, dashboards show gaps

This is why graceful shutdown matters.
```

## Step 5 -- Production-Ready Shutdown with signal.NotifyContext

Combine everything into the pattern you will use in every production Go service:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	simulatedRunTime      = 1200 * time.Millisecond
	gracefulShutdownLimit = 30 * time.Second
	httpRequestInterval   = 250 * time.Millisecond
	queuePollInterval     = 350 * time.Millisecond
	schedulerInterval     = 400 * time.Millisecond
	httpDrainTime         = 200 * time.Millisecond
	queueOffsetCommitTime = 100 * time.Millisecond
	schedulerStopTime     = 50 * time.Millisecond
	dbPoolCloseTime       = 50 * time.Millisecond
	logBufferFlushTime    = 30 * time.Millisecond
	metricsCloseTime      = 20 * time.Millisecond
)

type Service struct {
	wg sync.WaitGroup
}

func NewService() *Service {
	return &Service{}
}

func (s *Service) StartHTTP(ctx context.Context) {
	defer s.wg.Done()
	fmt.Println("[http] server started on :8080")
	ticker := time.NewTicker(httpRequestInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("[http] draining connections (5 in-flight)...")
			time.Sleep(httpDrainTime)
			fmt.Println("[http] all connections drained")
			return
		case <-ticker.C:
			fmt.Println("[http] served request")
		}
	}
}

func (s *Service) StartQueueConsumer(ctx context.Context) {
	defer s.wg.Done()
	fmt.Println("[queue] consumer started")
	ticker := time.NewTicker(queuePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("[queue] committing current offset...")
			time.Sleep(queueOffsetCommitTime)
			fmt.Println("[queue] offset committed, consumer stopped")
			return
		case <-ticker.C:
			fmt.Println("[queue] processed message")
		}
	}
}

func (s *Service) StartScheduler(ctx context.Context) {
	defer s.wg.Done()
	fmt.Println("[scheduler] started")
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("[scheduler] cancelling pending jobs...")
			time.Sleep(schedulerStopTime)
			fmt.Println("[scheduler] stopped")
			return
		case <-ticker.C:
			fmt.Println("[scheduler] ran scheduled task")
		}
	}
}

func (s *Service) WaitForWorkers(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func cleanupResources() {
	fmt.Println("\n[cleanup] closing database pool...")
	time.Sleep(dbPoolCloseTime)
	fmt.Println("[cleanup] flushing log buffer...")
	time.Sleep(logBufferFlushTime)
	fmt.Println("[cleanup] closing metrics exporter...")
	time.Sleep(metricsCloseTime)
}

func simulateShutdownSignal(delay time.Duration) {
	time.Sleep(delay)
	fmt.Println("\n[test] simulating SIGTERM...")
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGTERM)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc := NewService()

	svc.wg.Add(3)
	go svc.StartHTTP(ctx)
	go svc.StartQueueConsumer(ctx)
	go svc.StartScheduler(ctx)

	go simulateShutdownSignal(simulatedRunTime)

	<-ctx.Done()
	stop()

	fmt.Println("\n======================================")
	fmt.Println("[system] shutdown initiated")
	fmt.Println("======================================")

	fmt.Printf("[system] waiting up to %v for workers...\n\n", gracefulShutdownLimit)

	if svc.WaitForWorkers(gracefulShutdownLimit) {
		fmt.Println("\n[system] all workers stopped")
	} else {
		fmt.Println("\n[system] TIMEOUT: forcing exit (some workers did not stop)")
	}

	cleanupResources()

	fmt.Println("\n[system] graceful shutdown complete")
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
[http] server started on :8080
[queue] consumer started
[scheduler] started
[http] served request
[queue] processed message
[scheduler] ran scheduled task
[http] served request
[http] served request
[queue] processed message
[scheduler] ran scheduled task

[test] simulating SIGTERM...

======================================
[system] shutdown initiated
======================================
[system] waiting up to 30s for workers...

[http] draining connections (5 in-flight)...
[queue] committing current offset...
[scheduler] cancelling pending jobs...
[scheduler] stopped
[queue] offset committed, consumer stopped
[http] all connections drained

[system] all workers stopped

[cleanup] closing database pool...
[cleanup] flushing log buffer...
[cleanup] closing metrics exporter...

[system] graceful shutdown complete
```

This is the production pattern: `signal.NotifyContext` for signal handling, workers check `ctx.Done()` for shutdown, WaitGroup ensures all workers finish, timeout prevents hanging, and cleanup runs after workers stop.

## Common Mistakes

### Not Using a Shutdown Timeout
**Wrong:**
```go
rootCancel()
wg.Wait() // waits forever if a worker is stuck
```
**What happens:** If any worker hangs (network issue, deadlock), the service never exits. Container orchestrators will eventually SIGKILL it after 30 seconds, losing any progress the other workers made.

**Fix:** Always pair shutdown with a timeout:
```go
rootCancel()
done := make(chan struct{})
go func() { wg.Wait(); close(done) }()
select {
case <-done:       // workers finished
case <-time.After(timeout): // force exit
}
```

### Calling os.Exit Before Cleanup
**Wrong:**
```go
case <-sigChan:
    os.Exit(0) // no cleanup, in-flight work lost!
```
**Fix:** Translate the signal into a context cancellation and let the shutdown flow proceed. Only use `os.Exit` for the second-signal safety valve.

### WaitGroup Misuse
Two common errors:
1. Calling `wg.Add(1)` inside the goroutine instead of before launching it -- creates a race condition where `wg.Wait()` might return before the goroutine starts
2. Forgetting `defer wg.Done()` in the goroutine -- `wg.Wait()` blocks forever

### Forgetting to Stop Tickers and Timers
Tickers and timers that are not stopped continue consuming resources. Always `defer ticker.Stop()`. In a worker that runs for hours, a leaked ticker wastes CPU on every tick.

## Review

Graceful shutdown is a single idea applied consistently: turn an external
signal into a context cancellation, then let that cancellation flow down one
tree to every goroutine. `signal.NotifyContext` (Go 1.16+) gives you a context
that cancels on SIGINT or SIGTERM without hand-rolled channels; each worker
selects on `ctx.Done()` and, on cancellation, finishes its current unit of work
-- draining connections, committing an offset, flushing a buffer -- before
returning. A `sync.WaitGroup` lets main block until every worker has returned,
but never unconditionally: you wrap the wait in a `select` against a timeout so
a single stuck worker cannot hang the process past the orchestrator's own kill
deadline. Only after the workers stop do you close the database pool and flush
logs, in that order, so cleanup never races the work it depends on. Calling
`stop()` after the first signal restores default handling, so a second Ctrl+C
is a hard-kill safety valve.

To exercise the pattern end to end, build a mini-system: one producer feeding a
buffered channel and two consumers draining it, all watching a shared context.
Cancel after roughly 800ms and confirm the shutdown is graceful -- the producer
stops sending and closes the channel, each consumer reports how many items it
processed, and a WaitGroup guarded by a timeout lets main declare completion
rather than hang. If you find yourself tempted to call `os.Exit` on the signal,
or to `wg.Wait()` without a timeout, revisit the Common Mistakes: the first
drops in-flight work, the second reintroduces exactly the hang the timeout
exists to prevent.

## Resources
- [Package os/signal: NotifyContext](https://pkg.go.dev/os/signal#NotifyContext) -- the constructor that turns SIGINT/SIGTERM into a cancelable context.
- [Package os/signal](https://pkg.go.dev/os/signal) -- signal delivery and the Notify/Stop mechanics underneath NotifyContext.
- [Package syscall: SIGINT, SIGTERM](https://pkg.go.dev/syscall) -- the signal constants an orchestrator sends and this service listens for.
- [Go Blog: Context](https://go.dev/blog/context) -- how context cancellation propagates through a tree of goroutines.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-pipeline-pattern](../../07-concurrency-patterns/01-pipeline-pattern/01-pipeline-pattern.md)
