# Exercise 30: Goroutine DAG Orchestrator

Every build system, CI/CD pipeline, and workflow engine solves the same problem:
run tasks respecting dependency order while extracting the most parallelism
possible. Task C waits on A and B; D waits on C; A and B, sharing no dependency,
run at the same time. This capstone builds that engine -- a DAG orchestrator
that topologically sorts tasks, launches a goroutine only once every dependency
has completed, propagates failures so downstream tasks skip rather than run on
broken input, and computes the critical path that fixes the floor on how fast
the whole graph can finish.

## What you'll build

```text
30-goroutine-dag-orchestrator/
  main.go        DAG validation with Kahn's topological sort, an orchestrator
                 with error propagation, and critical-path analysis
```

- Build: a DAG task orchestrator that runs a simulated CI/CD pipeline with maximum safe parallelism.
- Implement: `DAG` with `TopologicalSort` (Kahn) and `ParallelLevels`, an `Orchestrator` with a ready-channel scheduler and `shouldSkip` error propagation, and `computeCriticalPath`.
- Verify: `go run main.go` on each step.

### Why dependency order and the critical path both matter

This is a capstone that integrates nearly everything from this section:
launching goroutines, coordinating with WaitGroups and channels, guarding shared
state with a mutex, handling errors, and designing goroutine ownership. The DAG
orchestrator is not just an exercise -- it is the pattern behind Make, Bazel,
GitHub Actions, Airflow, and Temporal, and behind configuration managers, data
pipeline builders, deployment orchestrators, and even UI rendering engines.

Critical-path analysis adds the practical dimension: given the DAG and per-task
durations, the critical path is the longest sequential chain, and it sets the
minimum possible execution time regardless of how many goroutines you throw at
the graph. Knowing it tells you exactly which tasks to optimize -- and which
ones give you nothing, because they already overlap something slower.


## Step 1 -- DAG Definition and Validation

Define the task graph, implement topological sort to determine valid execution order, and detect cycles that would cause deadlocks.

```go
package main

import (
	"fmt"
	"time"
)

type TaskStatus int

const (
	TaskPending   TaskStatus = iota
	TaskRunning
	TaskCompleted
	TaskFailed
	TaskSkipped
)

func (s TaskStatus) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskRunning:
		return "running"
	case TaskCompleted:
		return "completed"
	case TaskFailed:
		return "failed"
	case TaskSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

type TaskDef struct {
	Name     string
	Duration time.Duration
	DependsOn []string
	Fn       func() error
}

type DAG struct {
	tasks    map[string]TaskDef
	order    []string
}

func NewDAG() *DAG {
	return &DAG{tasks: make(map[string]TaskDef)}
}

func (d *DAG) AddTask(def TaskDef) {
	d.tasks[def.Name] = def
	d.order = append(d.order, def.Name)
}

func (d *DAG) Validate() error {
	for name, task := range d.tasks {
		for _, dep := range task.DependsOn {
			if _, ok := d.tasks[dep]; !ok {
				return fmt.Errorf("task %q depends on unknown task %q", name, dep)
			}
		}
	}

	_, err := d.TopologicalSort()
	return err
}

func (d *DAG) TopologicalSort() ([]string, error) {
	inDegree := make(map[string]int, len(d.tasks))
	dependents := make(map[string][]string, len(d.tasks))

	for name := range d.tasks {
		inDegree[name] = 0
	}
	for name, task := range d.tasks {
		for _, dep := range task.DependsOn {
			dependents[dep] = append(dependents[dep], name)
			inDegree[name]++
		}
	}

	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)

		for _, dependent := range dependents[node] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) != len(d.tasks) {
		return nil, fmt.Errorf("cycle detected: %d tasks in graph, only %d reachable",
			len(d.tasks), len(sorted))
	}

	return sorted, nil
}

func (d *DAG) ParallelLevels() [][]string {
	inDegree := make(map[string]int, len(d.tasks))
	dependents := make(map[string][]string, len(d.tasks))

	for name := range d.tasks {
		inDegree[name] = 0
	}
	for name, task := range d.tasks {
		for _, dep := range task.DependsOn {
			dependents[dep] = append(dependents[dep], name)
			inDegree[name]++
		}
	}

	var levels [][]string
	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	for len(queue) > 0 {
		levels = append(levels, queue)
		var nextQueue []string
		for _, node := range queue {
			for _, dependent := range dependents[node] {
				inDegree[dependent]--
				if inDegree[dependent] == 0 {
					nextQueue = append(nextQueue, dependent)
				}
			}
		}
		queue = nextQueue
	}

	return levels
}

func main() {
	dag := NewDAG()

	dag.AddTask(TaskDef{Name: "checkout", Duration: 100 * time.Millisecond})
	dag.AddTask(TaskDef{Name: "install-deps", Duration: 200 * time.Millisecond, DependsOn: []string{"checkout"}})
	dag.AddTask(TaskDef{Name: "lint", Duration: 150 * time.Millisecond, DependsOn: []string{"install-deps"}})
	dag.AddTask(TaskDef{Name: "unit-tests", Duration: 300 * time.Millisecond, DependsOn: []string{"install-deps"}})
	dag.AddTask(TaskDef{Name: "build", Duration: 250 * time.Millisecond, DependsOn: []string{"lint"}})
	dag.AddTask(TaskDef{Name: "integration-tests", Duration: 400 * time.Millisecond, DependsOn: []string{"build"}})
	dag.AddTask(TaskDef{Name: "security-scan", Duration: 200 * time.Millisecond, DependsOn: []string{"build"}})
	dag.AddTask(TaskDef{Name: "deploy", Duration: 150 * time.Millisecond, DependsOn: []string{"integration-tests", "security-scan", "unit-tests"}})

	if err := dag.Validate(); err != nil {
		fmt.Printf("DAG validation failed: %v\n", err)
		return
	}

	sorted, _ := dag.TopologicalSort()
	fmt.Println("=== Topological Order ===")
	for i, name := range sorted {
		task := dag.tasks[name]
		deps := "none"
		if len(task.DependsOn) > 0 {
			deps = fmt.Sprintf("%v", task.DependsOn)
		}
		fmt.Printf("  %d. %-20s deps=%s\n", i+1, name, deps)
	}

	levels := dag.ParallelLevels()
	fmt.Println()
	fmt.Println("=== Parallel Execution Levels ===")
	totalSequential := time.Duration(0)
	totalCriticalPath := time.Duration(0)
	for i, level := range levels {
		var maxDuration time.Duration
		for _, name := range level {
			d := dag.tasks[name].Duration
			totalSequential += d
			if d > maxDuration {
				maxDuration = d
			}
		}
		totalCriticalPath += maxDuration
		fmt.Printf("  Level %d: %v (max duration: %v)\n", i+1, level, maxDuration)
	}

	fmt.Printf("\n=== Time Analysis ===\n")
	fmt.Printf("  Sequential time: %v\n", totalSequential)
	fmt.Printf("  Parallel minimum: %v\n", totalCriticalPath)
	fmt.Printf("  Speedup: %.1fx\n", float64(totalSequential)/float64(totalCriticalPath))
}
```

**What's happening here:** The DAG represents a CI/CD pipeline with 8 tasks. `TopologicalSort` uses Kahn's algorithm: start with tasks that have no dependencies (in-degree zero), process them, reduce the in-degree of their dependents, and repeat. If the sorted output has fewer tasks than the graph, a cycle exists. `ParallelLevels` groups tasks that can execute simultaneously -- all tasks at the same level have their dependencies satisfied by previous levels.

**Key insight:** The parallel levels reveal the minimum execution time. Even with infinite goroutines, you cannot be faster than the sum of the maximum durations at each level. This is the critical path. In the CI pipeline, checkout -> install-deps -> lint -> build -> integration-tests -> deploy is the critical path (1200ms). Optimizing `security-scan` (200ms) provides zero benefit because it runs in parallel with `integration-tests` (400ms).

### Verification
```bash
go run main.go
```
Expected output:
```
=== Topological Order ===
  1. checkout             deps=none
  2. install-deps         deps=[checkout]
  3. lint                 deps=[install-deps]
  4. unit-tests           deps=[install-deps]
  5. build                deps=[lint]
  6. integration-tests    deps=[build]
  7. security-scan        deps=[build]
  8. deploy               deps=[integration-tests security-scan unit-tests]

=== Parallel Execution Levels ===
  Level 1: [checkout] (max duration: 100ms)
  Level 2: [install-deps] (max duration: 200ms)
  Level 3: [lint unit-tests] (max duration: 300ms)
  Level 4: [build] (max duration: 250ms)
  Level 5: [integration-tests security-scan] (max duration: 400ms)
  Level 6: [deploy] (max duration: 150ms)

=== Time Analysis ===
  Sequential time: 1.75s
  Parallel minimum: 1.4s
  Speedup: 1.2x
```


## Step 2 -- Concurrent Execution with Error Propagation

Build the execution engine that launches goroutines based on dependency completion and propagates errors to skip downstream tasks.

```go
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskRunning
	TaskCompleted
	TaskFailed
	TaskSkipped
)

func (s TaskStatus) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskRunning:
		return "running"
	case TaskCompleted:
		return "completed"
	case TaskFailed:
		return "FAILED"
	case TaskSkipped:
		return "SKIPPED"
	default:
		return "unknown"
	}
}

type TaskDef struct {
	Name      string
	Duration  time.Duration
	DependsOn []string
	Fn        func() error
}

type TaskResult struct {
	Name     string
	Status   TaskStatus
	Duration time.Duration
	Error    error
}

type Orchestrator struct {
	mu         sync.Mutex
	tasks      map[string]TaskDef
	status     map[string]TaskStatus
	results    map[string]TaskResult
	dependents map[string][]string
	pending    map[string]int
	readyCh    chan string
}

func NewOrchestrator() *Orchestrator {
	return &Orchestrator{
		tasks:      make(map[string]TaskDef),
		status:     make(map[string]TaskStatus),
		results:    make(map[string]TaskResult),
		dependents: make(map[string][]string),
		pending:    make(map[string]int),
		readyCh:    make(chan string, 64),
	}
}

func (o *Orchestrator) AddTask(def TaskDef) {
	o.tasks[def.Name] = def
	o.status[def.Name] = TaskPending
	o.pending[def.Name] = len(def.DependsOn)

	for _, dep := range def.DependsOn {
		o.dependents[dep] = append(o.dependents[dep], def.Name)
	}
}

func (o *Orchestrator) shouldSkip(name string) bool {
	task := o.tasks[name]
	for _, dep := range task.DependsOn {
		s := o.status[dep]
		if s == TaskFailed || s == TaskSkipped {
			return true
		}
	}
	return false
}

func (o *Orchestrator) taskDone(name string, result TaskResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.status[name] = result.Status
	o.results[name] = result

	for _, dependent := range o.dependents[name] {
		o.pending[dependent]--
		if o.pending[dependent] == 0 {
			o.readyCh <- dependent
		}
	}
}

func (o *Orchestrator) Execute() map[string]TaskResult {
	o.mu.Lock()
	totalTasks := len(o.tasks)
	for name, count := range o.pending {
		if count == 0 {
			o.readyCh <- name
		}
	}
	o.mu.Unlock()

	var wg sync.WaitGroup
	completed := 0

	for completed < totalTasks {
		name := <-o.readyCh

		o.mu.Lock()
		skip := o.shouldSkip(name)
		if skip {
			o.status[name] = TaskSkipped
			o.results[name] = TaskResult{Name: name, Status: TaskSkipped}
			for _, dependent := range o.dependents[name] {
				o.pending[dependent]--
				if o.pending[dependent] == 0 {
					o.readyCh <- dependent
				}
			}
			o.mu.Unlock()
			completed++
			continue
		}
		o.status[name] = TaskRunning
		o.mu.Unlock()

		wg.Add(1)
		go func(taskName string) {
			defer wg.Done()
			task := o.tasks[taskName]

			start := time.Now()
			var err error
			if task.Fn != nil {
				err = task.Fn()
			} else {
				time.Sleep(task.Duration)
			}
			elapsed := time.Since(start)

			result := TaskResult{
				Name:     taskName,
				Duration: elapsed,
			}
			if err != nil {
				result.Status = TaskFailed
				result.Error = err
			} else {
				result.Status = TaskCompleted
			}

			o.taskDone(taskName, result)
		}(name)
		completed++
	}

	wg.Wait()
	return o.results
}

func main() {
	orch := NewOrchestrator()

	orch.AddTask(TaskDef{Name: "checkout", Duration: 100 * time.Millisecond})
	orch.AddTask(TaskDef{Name: "install-deps", Duration: 200 * time.Millisecond, DependsOn: []string{"checkout"}})
	orch.AddTask(TaskDef{Name: "lint", Duration: 150 * time.Millisecond, DependsOn: []string{"install-deps"}})
	orch.AddTask(TaskDef{Name: "unit-tests", Duration: 300 * time.Millisecond, DependsOn: []string{"install-deps"}})
	orch.AddTask(TaskDef{Name: "build", Duration: 250 * time.Millisecond, DependsOn: []string{"lint"}})
	orch.AddTask(TaskDef{Name: "integration-tests", Duration: 400 * time.Millisecond, DependsOn: []string{"build"},
		Fn: func() error {
			time.Sleep(100 * time.Millisecond)
			return errors.New("test assertion failed: expected 200 got 500")
		},
	})
	orch.AddTask(TaskDef{Name: "security-scan", Duration: 200 * time.Millisecond, DependsOn: []string{"build"}})
	orch.AddTask(TaskDef{Name: "deploy", Duration: 150 * time.Millisecond, DependsOn: []string{"integration-tests", "security-scan", "unit-tests"}})

	fmt.Println("=== DAG Orchestrator: CI Pipeline ===")
	start := time.Now()
	results := orch.Execute()
	elapsed := time.Since(start)

	order := []string{"checkout", "install-deps", "lint", "unit-tests", "build",
		"integration-tests", "security-scan", "deploy"}

	fmt.Printf("\n  %-25s %-12s %-12s %s\n", "Task", "Status", "Duration", "Error")
	fmt.Println("  " + "----------------------------------------------------------------------")
	for _, name := range order {
		r := results[name]
		errMsg := ""
		if r.Error != nil {
			errMsg = r.Error.Error()
		}
		fmt.Printf("  %-25s %-12s %-12v %s\n",
			r.Name, r.Status, r.Duration.Round(time.Millisecond), errMsg)
	}

	var completed, failed, skipped int
	for _, r := range results {
		switch r.Status {
		case TaskCompleted:
			completed++
		case TaskFailed:
			failed++
		case TaskSkipped:
			skipped++
		}
	}

	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("  Wall time: %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Completed: %d | Failed: %d | Skipped: %d\n", completed, failed, skipped)

	if failed > 0 {
		fmt.Println()
		fmt.Println("  Pipeline FAILED: error propagation skipped downstream tasks")
	}
}
```

**What's happening here:** The orchestrator uses a `readyCh` channel to signal when a task's dependencies are satisfied. Initially, tasks with no dependencies are enqueued. When a task completes (or fails), `taskDone` decrements the pending count of its dependents. When a dependent's count reaches zero, it is enqueued to `readyCh`. The main loop reads from `readyCh`, checks if the task should be skipped (a dependency failed), and either skips it or launches a goroutine.

**Key insight:** Error propagation cascades through the DAG. When `integration-tests` fails, `deploy` is skipped because it depends on `integration-tests`. But `security-scan` still runs because it does not depend on `integration-tests` -- it depends on `build`, which succeeded. The orchestrator maximizes useful work: it does not abort the entire pipeline on first failure, only the affected downstream path.

### Verification
```bash
go run main.go
```
Expected output:
```
=== DAG Orchestrator: CI Pipeline ===

  Task                      Status       Duration     Error
  ----------------------------------------------------------------------
  checkout                  completed    100ms
  install-deps              completed    200ms
  lint                      completed    150ms
  unit-tests                completed    300ms
  build                     completed    250ms
  integration-tests         FAILED       100ms        test assertion failed: expected 200 got 500
  security-scan             completed    200ms
  deploy                    SKIPPED      0s

=== Summary ===
  Wall time: 903ms
  Completed: 6 | Failed: 1 | Skipped: 1

  Pipeline FAILED: error propagation skipped downstream tasks
```


## Step 3 -- Critical Path Analysis

Add critical path computation to identify which tasks determine the minimum execution time. Display the actual execution timeline alongside the theoretical critical path.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskRunning
	TaskCompleted
	TaskFailed
	TaskSkipped
)

func (s TaskStatus) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskRunning:
		return "running"
	case TaskCompleted:
		return "completed"
	case TaskFailed:
		return "FAILED"
	case TaskSkipped:
		return "SKIPPED"
	default:
		return "unknown"
	}
}

type TaskDef struct {
	Name      string
	Duration  time.Duration
	DependsOn []string
	Fn        func() error
}

type TaskResult struct {
	Name      string
	Status    TaskStatus
	Duration  time.Duration
	StartedAt time.Duration
	Error     error
}

type Orchestrator struct {
	mu         sync.Mutex
	tasks      map[string]TaskDef
	status     map[string]TaskStatus
	results    map[string]TaskResult
	dependents map[string][]string
	pending    map[string]int
	readyCh    chan string
	startTime  time.Time
}

func NewOrchestrator() *Orchestrator {
	return &Orchestrator{
		tasks:      make(map[string]TaskDef),
		status:     make(map[string]TaskStatus),
		results:    make(map[string]TaskResult),
		dependents: make(map[string][]string),
		pending:    make(map[string]int),
		readyCh:    make(chan string, 64),
	}
}

func (o *Orchestrator) AddTask(def TaskDef) {
	o.tasks[def.Name] = def
	o.status[def.Name] = TaskPending
	o.pending[def.Name] = len(def.DependsOn)
	for _, dep := range def.DependsOn {
		o.dependents[dep] = append(o.dependents[dep], def.Name)
	}
}

func (o *Orchestrator) shouldSkip(name string) bool {
	task := o.tasks[name]
	for _, dep := range task.DependsOn {
		s := o.status[dep]
		if s == TaskFailed || s == TaskSkipped {
			return true
		}
	}
	return false
}

func (o *Orchestrator) taskDone(name string, result TaskResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status[name] = result.Status
	o.results[name] = result
	for _, dependent := range o.dependents[name] {
		o.pending[dependent]--
		if o.pending[dependent] == 0 {
			o.readyCh <- dependent
		}
	}
}

func (o *Orchestrator) Execute() map[string]TaskResult {
	o.startTime = time.Now()

	o.mu.Lock()
	totalTasks := len(o.tasks)
	for name, count := range o.pending {
		if count == 0 {
			o.readyCh <- name
		}
	}
	o.mu.Unlock()

	var wg sync.WaitGroup
	completed := 0

	for completed < totalTasks {
		name := <-o.readyCh

		o.mu.Lock()
		skip := o.shouldSkip(name)
		if skip {
			o.status[name] = TaskSkipped
			o.results[name] = TaskResult{Name: name, Status: TaskSkipped, StartedAt: time.Since(o.startTime)}
			for _, dependent := range o.dependents[name] {
				o.pending[dependent]--
				if o.pending[dependent] == 0 {
					o.readyCh <- dependent
				}
			}
			o.mu.Unlock()
			completed++
			continue
		}
		o.status[name] = TaskRunning
		o.mu.Unlock()

		wg.Add(1)
		go func(taskName string) {
			defer wg.Done()
			task := o.tasks[taskName]
			startedAt := time.Since(o.startTime)
			start := time.Now()

			var err error
			if task.Fn != nil {
				err = task.Fn()
			} else {
				time.Sleep(task.Duration)
			}
			elapsed := time.Since(start)

			result := TaskResult{Name: taskName, Duration: elapsed, StartedAt: startedAt}
			if err != nil {
				result.Status = TaskFailed
				result.Error = err
			} else {
				result.Status = TaskCompleted
			}
			o.taskDone(taskName, result)
		}(name)
		completed++
	}

	wg.Wait()
	return o.results
}

func computeCriticalPath(tasks map[string]TaskDef) ([]string, time.Duration) {
	earliest := make(map[string]time.Duration, len(tasks))
	predecessor := make(map[string]string, len(tasks))

	inDegree := make(map[string]int)
	dependents := make(map[string][]string)
	for name := range tasks {
		inDegree[name] = 0
	}
	for name, task := range tasks {
		for _, dep := range task.DependsOn {
			dependents[dep] = append(dependents[dep], name)
			inDegree[name]++
		}
	}

	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
			earliest[name] = tasks[name].Duration
		}
	}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		endTime := earliest[node]

		for _, dependent := range dependents[node] {
			candidateStart := endTime + tasks[dependent].Duration
			if candidateStart > earliest[dependent] {
				earliest[dependent] = candidateStart
				predecessor[dependent] = node
			}
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	var criticalEnd string
	var maxTime time.Duration
	for name, t := range earliest {
		if t > maxTime {
			maxTime = t
			criticalEnd = name
		}
	}

	var path []string
	current := criticalEnd
	for current != "" {
		path = append([]string{current}, path...)
		current = predecessor[current]
	}

	return path, maxTime
}

func main() {
	orch := NewOrchestrator()

	orch.AddTask(TaskDef{Name: "checkout", Duration: 100 * time.Millisecond})
	orch.AddTask(TaskDef{Name: "install-deps", Duration: 200 * time.Millisecond, DependsOn: []string{"checkout"}})
	orch.AddTask(TaskDef{Name: "lint", Duration: 150 * time.Millisecond, DependsOn: []string{"install-deps"}})
	orch.AddTask(TaskDef{Name: "unit-tests", Duration: 300 * time.Millisecond, DependsOn: []string{"install-deps"}})
	orch.AddTask(TaskDef{Name: "build", Duration: 250 * time.Millisecond, DependsOn: []string{"lint"}})
	orch.AddTask(TaskDef{Name: "integration-tests", Duration: 400 * time.Millisecond, DependsOn: []string{"build"}})
	orch.AddTask(TaskDef{Name: "security-scan", Duration: 200 * time.Millisecond, DependsOn: []string{"build"}})
	orch.AddTask(TaskDef{Name: "deploy", Duration: 150 * time.Millisecond, DependsOn: []string{"integration-tests", "security-scan", "unit-tests"}})

	criticalPath, criticalTime := computeCriticalPath(orch.tasks)

	fmt.Println("=== DAG Orchestrator with Critical Path ===")
	fmt.Println()
	fmt.Printf("Critical path: %v\n", criticalPath)
	fmt.Printf("Critical path time: %v\n\n", criticalTime)

	fmt.Println("--- Executing Pipeline ---")
	start := time.Now()
	results := orch.Execute()
	elapsed := time.Since(start)

	order := []string{"checkout", "install-deps", "lint", "unit-tests", "build",
		"integration-tests", "security-scan", "deploy"}

	fmt.Printf("\n  %-25s %-12s %-12s %-12s\n", "Task", "Status", "Started", "Duration")
	fmt.Println("  " + "--------------------------------------------------------------")
	for _, name := range order {
		r := results[name]
		fmt.Printf("  %-25s %-12s %-12v %-12v\n",
			r.Name, r.Status,
			r.StartedAt.Round(time.Millisecond),
			r.Duration.Round(time.Millisecond))
	}

	fmt.Printf("\n=== Execution Analysis ===\n")
	fmt.Printf("  Critical path time: %v (theoretical minimum)\n", criticalTime)
	fmt.Printf("  Actual wall time:   %v\n", elapsed.Round(time.Millisecond))
	overhead := float64(elapsed-criticalTime) / float64(criticalTime) * 100
	fmt.Printf("  Scheduling overhead: %.1f%%\n", overhead)

	var totalWork time.Duration
	for _, r := range results {
		totalWork += r.Duration
	}
	parallelism := float64(totalWork) / float64(elapsed)
	fmt.Printf("  Effective parallelism: %.1fx\n", parallelism)

	fmt.Printf("\n=== Critical Path Detail ===\n")
	pathTime := time.Duration(0)
	for _, name := range criticalPath {
		task := orch.tasks[name]
		pathTime += task.Duration
		fmt.Printf("  %-25s %v (cumulative: %v)\n", name, task.Duration, pathTime)
	}

	fmt.Println()
	fmt.Println("  Tasks NOT on critical path can be optimized only up to the critical path time.")
	fmt.Println("  To reduce wall time, optimize tasks ON the critical path.")
}
```

**What's happening here:** `computeCriticalPath` performs a forward pass through the DAG, computing the earliest completion time for each task. The task with the latest completion time is the end of the critical path. Backtracking through predecessors reconstructs the full path. The execution engine tracks `StartedAt` timestamps to show when each task actually began, revealing how parallelism played out.

**Key insight:** The critical path determines the minimum possible execution time. In this pipeline, the critical path is `checkout -> install-deps -> lint -> build -> integration-tests -> deploy` (1250ms). `unit-tests` (300ms) and `security-scan` (200ms) run in parallel with other tasks and do not affect the critical path. Even if you made `security-scan` ten times faster, the wall time would not change. This is the most important insight for pipeline optimization: find the critical path first, then optimize only those tasks.

### Verification
```bash
go run main.go
```
Expected output:
```
=== DAG Orchestrator with Critical Path ===

Critical path: [checkout install-deps lint build integration-tests deploy]
Critical path time: 1.25s

--- Executing Pipeline ---

  Task                      Status       Started      Duration
  --------------------------------------------------------------
  checkout                  completed    0s           100ms
  install-deps              completed    100ms        200ms
  lint                      completed    300ms        150ms
  unit-tests                completed    300ms        300ms
  build                     completed    450ms        250ms
  integration-tests         completed    700ms        400ms
  security-scan             completed    700ms        200ms
  deploy                    completed    1.1s         150ms

=== Execution Analysis ===
  Critical path time: 1.25s (theoretical minimum)
  Actual wall time:   1.254s
  Scheduling overhead: 0.3%
  Effective parallelism: 1.4x

=== Critical Path Detail ===
  checkout                  100ms (cumulative: 100ms)
  install-deps              200ms (cumulative: 300ms)
  lint                      150ms (cumulative: 450ms)
  build                     250ms (cumulative: 700ms)
  integration-tests         400ms (cumulative: 1.1s)
  deploy                    150ms (cumulative: 1.25s)

  Tasks NOT on critical path can be optimized only up to the critical path time.
  To reduce wall time, optimize tasks ON the critical path.
```


## Common Mistakes

### Not Detecting Cycles in the DAG

```go
// Wrong: no cycle detection, leads to deadlock
func (o *Orchestrator) Execute() {
	for name := range o.tasks {
		go func(n string) {
			// wait for all dependencies
			for _, dep := range o.tasks[n].DependsOn {
				<-o.doneCh[dep] // if A depends on B and B depends on A, deadlock
			}
			o.run(n)
		}(name)
	}
}
```
**What happens:** If task A depends on B and B depends on A (a cycle), both goroutines wait forever for each other. The orchestrator hangs silently. In a CI system, this manifests as a pipeline that runs indefinitely and must be manually killed.

**Fix:** Validate the DAG with topological sort before execution. If the sort visits fewer nodes than exist in the graph, a cycle exists. Report the cycle and refuse to execute.


### Launching All Goroutines at Once Without Dependency Checks

```go
// Wrong: all tasks start immediately regardless of dependencies
func (o *Orchestrator) ExecuteBroken() {
	var wg sync.WaitGroup
	for name, task := range o.tasks {
		wg.Add(1)
		go func(n string, t TaskDef) {
			defer wg.Done()
			time.Sleep(t.Duration) // runs immediately, no dependency wait
		}(name, task)
	}
	wg.Wait()
}
```
**What happens:** All 8 tasks start at t=0. `deploy` runs before `integration-tests` finishes. `build` runs before `lint` finishes. The dependency ordering is completely ignored. Results are meaningless because tasks consumed input from incomplete predecessors.

**Fix:** Use the ready-channel pattern: only enqueue a task when all its dependencies have completed. The orchestrator tracks pending dependency counts and signals readiness through a channel.


### Not Propagating Errors to Dependent Tasks

```go
// Wrong: downstream tasks run even when upstream fails
func (o *Orchestrator) taskDone(name string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.status[name] = TaskFailed
	} else {
		o.status[name] = TaskCompleted
	}
	// unlock dependents unconditionally
	for _, dep := range o.dependents[name] {
		o.pending[dep]--
		if o.pending[dep] == 0 {
			o.readyCh <- dep // deploy runs even though integration-tests failed
		}
	}
}
```
**What happens:** `deploy` runs after `integration-tests` fails, deploying broken code to production. The orchestrator treated failure as completion, satisfying the dependency without checking the outcome. This is the most dangerous bug in a pipeline orchestrator.

**Fix:** Before launching a task, check if any of its dependencies failed or were skipped. If so, mark the task as skipped and propagate the skip to its own dependents. The `shouldSkip` check in the execution loop prevents any task from running with a failed upstream.


## Review

The orchestrator turns a dependency graph into a schedule. Kahn's algorithm does
double duty -- it produces a valid execution order and, when it reaches fewer
nodes than the graph holds, proves a cycle exists, so you refuse to run a
pipeline that would otherwise deadlock silently. Execution itself is driven by a
ready channel: a task is enqueued only when its pending-dependency count hits
zero, which is what keeps `build` from starting before `lint` finishes. Failure
is data, not an exception -- when a task fails or is skipped, `shouldSkip` marks
its dependents as skipped and propagates that downstream, so a failed
`integration-tests` skips `deploy` while `security-scan`, which depends only on
`build`, still runs. And the critical path, computed by a forward pass over the
same graph, is the longest chain of durations: it is the wall-clock floor, and
optimizing anything off it buys nothing.

To be sure the model is yours, extend it. Give `TaskDef` a `MaxRetries int` and
wrap task execution in a loop that re-runs a failed task while retries remain
before marking it failed. Add an `OnlyIf func() bool` evaluated before the
goroutine launches: if it returns false, mark the task `TaskCompleted` -- a
successful no-op, so dependents still run -- rather than skipped. Then model a
`deploy-staging` that always runs and a `deploy-production` guarded by `OnlyIf:
func() bool { return isMainBranch }`, and print a report showing retries
attempted and conditions evaluated. If production deploys only on the main branch
while its dependents stay unaffected on other branches, the skip-versus-no-op
distinction has landed.


## Resources
- [Topological Sort (Kahn's Algorithm)](https://en.wikipedia.org/wiki/Topological_sorting#Kahn's_algorithm) -- the in-degree queue algorithm the validator and scheduler both use.
- [Critical Path Method](https://en.wikipedia.org/wiki/Critical_path_method) -- the project-scheduling technique behind computeCriticalPath.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- how the engine waits for every launched task goroutine to finish.
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- channel-based coordination patterns that underpin the ready-channel scheduler.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-unbuffered-channel-basics](../../02-channels/01-unbuffered-channel-basics/01-unbuffered-channel-basics.md)
