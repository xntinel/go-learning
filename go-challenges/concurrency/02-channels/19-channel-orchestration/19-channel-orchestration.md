# Exercise 19: Channel Orchestration

Deploying an application is not a flat list of steps. Some tasks depend on
others -- you cannot start the app until the Docker image is pulled and the
database migration has run -- while others, like pulling the image and running
the migration, are independent and should run in parallel to keep the deploy
fast. This exercise encodes that dependency graph directly in channels: each
task closes a "done" channel when it finishes, and dependent tasks receive from
their dependencies before starting. No polling, no shared flags, no timing
assumptions.

## What you'll build

```text
19-channel-orchestration/
  main.go        done-channel signaling, sequential chains, a diamond
                 dependency, and a deploy orchestrator with a timeline
```

- Build: a deployment orchestrator that runs tasks in dependency order using done channels.
- Implement: `RunTask` that waits on a `DependsOn` slice of receive-only channels then closes its own; parallel, sequential, and diamond graphs; an `Orchestrator` that records start/end times and prints a timeline.
- Verify: `go run main.go`, and `go run -race main.go` for the orchestrator step.

### Why a closed channel is the dependency edge

Channels model these dependencies naturally. Each task gets a "done" channel.
When a task finishes, it closes its done channel. Dependent tasks receive from
their dependency channels before starting -- the receive blocks until the
channel is closed. No polling, no shared flags, no timing assumptions. The
dependency graph is encoded directly in channel relationships.

This pattern scales from simple sequential chains to complex DAGs (directed
acyclic graphs) where multiple branches run in parallel and converge at
synchronization points.

## Step 1 -- Two Independent Tasks in Parallel

Start with the simplest case: two tasks with no dependencies run concurrently. Each task closes its "done" channel when finished. The main goroutine waits for both.

```go
package main

import (
	"fmt"
	"time"
)

const (
	pullImageDuration = 300 * time.Millisecond
	runMigrationDuration = 200 * time.Millisecond
)

// DeployTask represents a deployment step with a name and simulated duration.
type DeployTask struct {
	Name     string
	Duration time.Duration
}

// RunTask executes a task and closes its done channel when finished.
// The epoch parameter is used to print timestamps relative to deployment start.
func RunTask(task DeployTask, done chan struct{}, epoch time.Time) {
	start := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] START  %s\n", start, task.Name)

	time.Sleep(task.Duration)

	end := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] DONE   %s\n", end, task.Name)
	close(done)
}

func main() {
	epoch := time.Now()

	pullDone := make(chan struct{})
	migrateDone := make(chan struct{})

	pullImage := DeployTask{Name: "pull-docker-image", Duration: pullImageDuration}
	runMigration := DeployTask{Name: "run-db-migration", Duration: runMigrationDuration}

	go RunTask(pullImage, pullDone, epoch)
	go RunTask(runMigration, migrateDone, epoch)

	<-pullDone
	<-migrateDone

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] ALL TASKS COMPLETE\n", elapsed)
}
```

Key observations:
- Both tasks start at approximately the same time (~0ms)
- Total time is the maximum of the two durations, not the sum
- `close(done)` is the signal -- receiving from a closed channel returns immediately
- Closing (rather than sending a value) allows multiple goroutines to wait on the same done channel

### Verification
```bash
go run main.go
# Expected: both tasks start near 0ms, total ~300ms (not 500ms)
#   [   0s] START  pull-docker-image
#   [   0s] START  run-db-migration
#   [ 200ms] DONE   run-db-migration
#   [ 300ms] DONE   pull-docker-image
#   [ 300ms] ALL TASKS COMPLETE
```

## Step 2 -- Three Tasks Chained Sequentially

Now model strict sequential dependencies: pull image, then migrate, then start app. Each task waits for its predecessor's done channel before beginning.

```go
package main

import (
	"fmt"
	"time"
)

const (
	seqPullDuration    = 200 * time.Millisecond
	seqMigrateDuration = 150 * time.Millisecond
	seqStartDuration   = 100 * time.Millisecond
)

// DeployTask represents a deployment step with dependencies.
type DeployTask struct {
	Name      string
	Duration  time.Duration
	DependsOn []<-chan struct{}
}

// RunTask waits for all dependency channels to close, executes the task,
// then closes its own done channel.
func RunTask(task DeployTask, done chan struct{}, epoch time.Time) {
	for _, dep := range task.DependsOn {
		<-dep
	}

	start := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] START  %s\n", start, task.Name)

	time.Sleep(task.Duration)

	end := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] DONE   %s\n", end, task.Name)
	close(done)
}

func main() {
	epoch := time.Now()

	pullDone := make(chan struct{})
	migrateDone := make(chan struct{})
	startDone := make(chan struct{})

	pull := DeployTask{
		Name:     "pull-image",
		Duration: seqPullDuration,
	}
	migrate := DeployTask{
		Name:      "run-migration",
		Duration:  seqMigrateDuration,
		DependsOn: []<-chan struct{}{pullDone},
	}
	startApp := DeployTask{
		Name:      "start-app",
		Duration:  seqStartDuration,
		DependsOn: []<-chan struct{}{migrateDone},
	}

	go RunTask(pull, pullDone, epoch)
	go RunTask(migrate, migrateDone, epoch)
	go RunTask(startApp, startDone, epoch)

	<-startDone

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] DEPLOYMENT COMPLETE\n", elapsed)
}
```

All three goroutines are launched immediately, but the dependency channels ensure correct ordering: migrate waits for pull, start-app waits for migrate. Total time is the sum of all three durations.

### Verification
```bash
go run main.go
# Expected: sequential chain, total ~450ms
#   [   0s] START  pull-image
#   [ 200ms] DONE   pull-image
#   [ 200ms] START  run-migration
#   [ 350ms] DONE   run-migration
#   [ 350ms] START  start-app
#   [ 450ms] DONE   start-app
#   [ 450ms] DEPLOYMENT COMPLETE
```

## Step 3 -- Diamond Dependency: Parallel Branches Converge

The real deployment pattern: pull image (A) and seed cache (B) have no dependencies and run in parallel. Run migration (C) depends only on A. Start app (D) depends on both B and C. This forms a diamond shape.

```go
package main

import (
	"fmt"
	"time"
)

const (
	diamondPullDuration    = 300 * time.Millisecond
	diamondCacheDuration   = 200 * time.Millisecond
	diamondMigrateDuration = 150 * time.Millisecond
	diamondStartDuration   = 100 * time.Millisecond
)

// DeployTask represents a deployment step with a name, duration, and dependencies.
type DeployTask struct {
	Name      string
	Duration  time.Duration
	DependsOn []<-chan struct{}
}

// RunTask waits for all dependencies, executes, and signals completion.
func RunTask(task DeployTask, done chan struct{}, epoch time.Time) {
	for _, dep := range task.DependsOn {
		<-dep
	}

	start := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] START  %s\n", start, task.Name)

	time.Sleep(task.Duration)

	end := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] DONE   %s\n", end, task.Name)
	close(done)
}

func main() {
	epoch := time.Now()

	// Dependency graph (diamond):
	//
	//   pull-image (A) ----> run-migration (C) ---+
	//                                              +--> start-app (D)
	//   seed-cache (B) --------------------------+

	pullDone := make(chan struct{})
	cacheDone := make(chan struct{})
	migrateDone := make(chan struct{})
	startDone := make(chan struct{})

	pull := DeployTask{Name: "pull-image", Duration: diamondPullDuration}
	cache := DeployTask{Name: "seed-cache", Duration: diamondCacheDuration}
	migrate := DeployTask{
		Name:      "run-migration",
		Duration:  diamondMigrateDuration,
		DependsOn: []<-chan struct{}{pullDone},
	}
	startApp := DeployTask{
		Name:      "start-app",
		Duration:  diamondStartDuration,
		DependsOn: []<-chan struct{}{migrateDone, cacheDone},
	}

	go RunTask(pull, pullDone, epoch)
	go RunTask(cache, cacheDone, epoch)
	go RunTask(migrate, migrateDone, epoch)
	go RunTask(startApp, startDone, epoch)

	<-startDone

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("[%6s] DEPLOYMENT COMPLETE\n", elapsed)
	fmt.Println()
	fmt.Println("Dependency graph:")
	fmt.Println("  pull-image (A) --> run-migration (C) --+")
	fmt.Println("                                          +--> start-app (D)")
	fmt.Println("  seed-cache (B) ------------------------+")
}
```

The critical path is A -> C -> D (300 + 150 + 100 = 550ms). B (200ms) runs in parallel with A and finishes well before D needs it. Total deployment time follows the critical path, not the sum of all tasks.

### Verification
```bash
go run main.go
# Expected: A and B start at 0ms, C starts at ~300ms, D starts at ~450ms
# Total ~550ms (critical path: A -> C -> D)
```

## Step 4 -- Full Deployment with Timeline Report

Build a complete deployment orchestrator that records actual start/end times and prints a timeline proving the dependency ordering was respected.

```go
package main

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	taskPullImage     = 250 * time.Millisecond
	taskSeedCache     = 180 * time.Millisecond
	taskRunMigration  = 200 * time.Millisecond
	taskHealthCheck   = 80 * time.Millisecond
	taskStartApp      = 120 * time.Millisecond
	taskConfigureProxy = 60 * time.Millisecond
)

// TaskRecord captures the actual execution timeline of a task.
type TaskRecord struct {
	Name  string
	Start time.Duration
	End   time.Duration
}

// DeployTask describes a task with its dependencies.
type DeployTask struct {
	Name      string
	Duration  time.Duration
	DependsOn []<-chan struct{}
}

// Orchestrator manages task execution and collects timeline records.
type Orchestrator struct {
	epoch   time.Time
	mu      sync.Mutex
	records []TaskRecord
}

// NewOrchestrator creates an orchestrator with the given reference epoch.
func NewOrchestrator(epoch time.Time) *Orchestrator {
	return &Orchestrator{epoch: epoch}
}

// Launch starts a task in a new goroutine, returning its done channel.
func (o *Orchestrator) Launch(task DeployTask) chan struct{} {
	done := make(chan struct{})
	go func() {
		for _, dep := range task.DependsOn {
			<-dep
		}

		startOffset := time.Since(o.epoch)
		fmt.Printf("[%6s] START  %s\n", startOffset.Round(time.Millisecond), task.Name)

		time.Sleep(task.Duration)

		endOffset := time.Since(o.epoch)
		fmt.Printf("[%6s] DONE   %s\n", endOffset.Round(time.Millisecond), task.Name)

		o.mu.Lock()
		o.records = append(o.records, TaskRecord{
			Name:  task.Name,
			Start: startOffset.Round(time.Millisecond),
			End:   endOffset.Round(time.Millisecond),
		})
		o.mu.Unlock()

		close(done)
	}()
	return done
}

// PrintTimeline displays all tasks sorted by start time.
func (o *Orchestrator) PrintTimeline() {
	o.mu.Lock()
	sorted := make([]TaskRecord, len(o.records))
	copy(sorted, o.records)
	o.mu.Unlock()

	slices.SortFunc(sorted, func(a, b TaskRecord) int {
		return cmp.Compare(a.Start, b.Start)
	})

	fmt.Println()
	fmt.Println("=== Deployment Timeline ===")
	fmt.Printf("%-20s %10s %10s %10s\n", "TASK", "START", "END", "DURATION")
	fmt.Println("-------------------------------------------------------")
	for _, r := range sorted {
		fmt.Printf("%-20s %10s %10s %10s\n", r.Name, r.Start, r.End, r.End-r.Start)
	}
}

func main() {
	epoch := time.Now()
	orch := NewOrchestrator(epoch)

	// Dependency graph:
	//
	//   pull-image -----> run-migration ---+
	//        |                              +--> start-app --> configure-proxy
	//        +------> health-check         |
	//                                      |
	//   seed-cache -----------------------+

	pullDone := orch.Launch(DeployTask{
		Name: "pull-image", Duration: taskPullImage,
	})
	cacheDone := orch.Launch(DeployTask{
		Name: "seed-cache", Duration: taskSeedCache,
	})
	migrateDone := orch.Launch(DeployTask{
		Name: "run-migration", Duration: taskRunMigration,
		DependsOn: []<-chan struct{}{pullDone},
	})
	_ = orch.Launch(DeployTask{
		Name: "health-check", Duration: taskHealthCheck,
		DependsOn: []<-chan struct{}{pullDone},
	})
	startDone := orch.Launch(DeployTask{
		Name: "start-app", Duration: taskStartApp,
		DependsOn: []<-chan struct{}{migrateDone, cacheDone},
	})
	proxyDone := orch.Launch(DeployTask{
		Name: "configure-proxy", Duration: taskConfigureProxy,
		DependsOn: []<-chan struct{}{startDone},
	})

	<-proxyDone

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("\n[%6s] DEPLOYMENT COMPLETE\n", elapsed)
	orch.PrintTimeline()
}
```

### Verification
```bash
go run -race main.go
# Expected: timeline shows correct dependency ordering
# pull-image and seed-cache start at ~0ms
# run-migration and health-check start after pull-image completes (~250ms)
# start-app starts after both run-migration and seed-cache complete (~450ms)
# configure-proxy starts after start-app completes (~570ms)
# No race warnings
```

## Common Mistakes

### Sending a Value Instead of Closing the Done Channel

**Wrong:**
```go
done := make(chan struct{})
go func() {
    doWork()
    done <- struct{}{} // only one receiver gets the signal
}()
```

**What happens:** If two tasks depend on the same done channel, only one receives the signal. The other blocks forever.

**Fix:** Always close the done channel. A closed channel returns the zero value to all receivers, forever:
```go
done := make(chan struct{})
go func() {
    doWork()
    close(done) // all receivers unblock immediately
}()
```

### Circular Dependencies

**Wrong:**
```go
aDone := make(chan struct{})
bDone := make(chan struct{})
// A depends on B, B depends on A -- deadlock
go RunTask(DeployTask{DependsOn: []<-chan struct{}{bDone}}, aDone, epoch)
go RunTask(DeployTask{DependsOn: []<-chan struct{}{aDone}}, bDone, epoch)
```

**What happens:** Both goroutines block waiting for each other. Go's runtime detects the deadlock if no other goroutines are running.

**Fix:** Dependency graphs must be acyclic. Draw the graph before coding it. If you have cycles, restructure the tasks.

### Closing a Channel Twice

**Wrong:**
```go
done := make(chan struct{})
close(done)
close(done) // panic: close of closed channel
```

**Fix:** The task that creates the done channel closes it exactly once. Use the `RunTask` pattern where close is the last statement.

## Review

Each task owns one `chan struct{}` that it closes as its final act, and a
dependent task simply receives from every channel in its `DependsOn` list before
it begins. Closing rather than sending is what makes this compose: a closed
channel yields the zero value to every receiver forever, so any number of
downstream tasks can wait on the same upstream signal without competing for it.
Because independent tasks launch immediately and only block on the edges you
declare, wall-clock time collapses to the critical path through the graph rather
than the sum of all durations -- the diamond makes this visible, where the
A->C->D chain sets the total while B finishes in its shadow. The `DependsOn`
slice generalizes the whole idea to arbitrary DAGs.

You should be able to answer the three questions the exercise leaves you with.
Why close the done channel instead of sending a value through it -- what breaks
the moment two tasks depend on the same predecessor? What determines total
deployment time in a diamond, and why is it the critical path rather than the
slowest single task or the sum? And when `RunTask` waits on several
dependencies, does the order it receives them in matter, given that each receive
returns only once its channel is closed? The one rule that keeps all of this
honest: draw the graph before you code it, because a cycle in `DependsOn` is a
deadlock, not a slow deploy.

## Resources
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how stages signal completion and shut down, the same done-channel discipline scaled to streaming data.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the slide deck introducing done channels and the composition of concurrent tasks.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- channel basics and the closing idiom this orchestrator relies on.

---

Back to [Concurrency](../../concurrency.md) | Next: [20-bounded-work-queue](../20-bounded-work-queue/20-bounded-work-queue.md)
