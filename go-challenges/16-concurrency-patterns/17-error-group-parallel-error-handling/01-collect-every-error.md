# Exercise 1: Collect Every Error From Parallel Tasks

`errgroup.Group.Wait` returns the first error and cancels the rest. That is the wrong contract for a validator that must report every malformed field, a health check that must surface every dead dependency, or any batch where each failure should be visible. This exercise builds the opposite tool: a thread-safe `Collector` that records every labelled error, an errgroup-backed `RunAll` that bounds concurrency but never aborts a sibling, and an aggregated result built on `errors.Join` so `errors.Is` and `errors.As` reach every collected cause, not just the first.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
collector.go         Collector (mutex + errors.Join), Task, RunAll (errgroup.SetLimit), ErrDuplicate
cmd/
  demo/
    main.go          run five checks under a concurrency limit, print every failure
collector_test.go    nil is ignored, label wrapping, errors.Is reaches every cause, race-free Add, RunAll
```

- Files: `collector.go`, `cmd/demo/main.go`, `collector_test.go`.
- Implement: `Collector` with `Add`, `Len`, `Err`; the `Task` type; and `RunAll(tasks, limit) error`.
- Test: `collector_test.go` proves `Add(nil)` is ignored, the label is wrapped with `%w`, the joined error matches every collected cause, concurrent `Add` is race-free, and `RunAll` collects all failures.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p collect-every-error/cmd/demo && cd collect-every-error
go mod init example.com/collect-all
```

### Why `errgroup.Wait` is the wrong tool here, and what replaces it

`errgroup.Group.Wait` returns the first non-nil error any task produced, and when the group was built with `errgroup.WithContext` that first error also cancels the shared context, so the remaining tasks are torn down. That is exactly right when any failure invalidates the whole operation: a multi-step deploy, a migration, a fan-out where one bad shard means the answer is wrong anyway. It is exactly wrong when the caller's job is to produce a complete report. A form validator that stops at the first bad field forces the user through one round-trip per error; a startup health check that returns the first dead dependency hides the other four that are also down.

The replacement keeps errgroup for what it is genuinely good at — bounding the number of in-flight goroutines and joining them — and moves error handling out of the return value. Every task returns `nil` to the group (so the group never cancels), and its real error is handed to a `Collector` instead. The collector is the only shared mutable state, so it owns a mutex; `Add` is the single mutator and the read paths take the same lock. When every task has finished, `Err` folds the collected errors into one value with `errors.Join`.

The choice of `errors.Join` over a hand-rolled aggregator is the second half of the lesson. `errors.Join` (Go 1.20+) returns an error whose `Unwrap` method returns `[]error` — the whole slice, not a single next link. That plural `Unwrap` is what `errors.Is` and `errors.As` iterate over, so a sentinel buried in the *third* collected failure is still found by `errors.Is(joined, sentinel)`. A naive aggregator that exposes only the first error's chain (a single-valued `Unwrap`) silently fails that test: `errors.Is` never sees the other causes. Wrapping each entry with `fmt.Errorf("%s: %w", task, err)` before joining keeps both properties at once — the human-readable task label *and* the machine-readable chain down to the original error.

The trade-off to state out loud: because no task aborts a sibling, `RunAll` cannot be used for critical-path work where a failure should stop the others. It is a diagnostics tool. If you need cancellation, you want `errgroup.WithContext` and a returned error, not this collector.

Create `collector.go`:

```go
package collect

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"
)

// ErrDuplicate is a sentinel a task may return. RunAll's joined error still
// matches it with errors.Is even when it was not the first failure to occur.
var ErrDuplicate = errors.New("duplicate entry")

// Collector accumulates labelled errors from concurrent tasks. Every method is
// safe for concurrent use; Add is the only mutator.
type Collector struct {
	mu   sync.Mutex
	errs []error
}

// Add records err under the given task label. A nil err is ignored, so a caller
// can write c.Add(name, task()) with no nil check. The error is wrapped with %w
// so errors.Is and errors.As can still reach the original cause through the
// label.
func (c *Collector) Add(task string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, fmt.Errorf("%s: %w", task, err))
}

// Len reports how many errors have been collected.
func (c *Collector) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.errs)
}

// Err returns the collected errors folded into one value, or nil if none were
// collected. errors.Join exposes every entry via Unwrap []error, so the result
// matches every collected cause with errors.Is, not only the first.
func (c *Collector) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(c.errs...)
}

// Task is a named unit of work.
type Task struct {
	Name string
	Fn   func() error
}

// RunAll runs every task with at most limit running at once, lets each run to
// completion regardless of sibling failures, and returns one joined error (nil
// if all succeeded). It uses errgroup only to bound concurrency and join the
// goroutines: each task returns nil to the group so the group never cancels,
// and the real error is handed to the Collector. Use this for diagnostics
// (validation, health checks), never for critical-path work.
func RunAll(tasks []Task, limit int) error {
	var g errgroup.Group
	g.SetLimit(limit)
	var c Collector
	for _, t := range tasks {
		g.Go(func() error {
			c.Add(t.Name, t.Fn())
			return nil // collect, never abort a sibling
		})
	}
	_ = g.Wait() // always nil: tasks return nil, so the group cannot cancel
	return c.Err()
}
```

`g.SetLimit(limit)` caps the number of goroutines `g.Go` will run at once; the `g.Go` call blocks when the limit is reached and resumes when a slot frees, which is how the bound is enforced without a hand-written semaphore. The loop variable `t` is per-iteration under Go 1.22+ scoping, so the closure captures the right task with no `t := t` shadow. `_ = g.Wait()` is deliberate: the return is provably nil here, and the meaningful result is `c.Err()`.

### The runnable demo

The demo runs five checks under a concurrency limit of three. Two pass, three fail — one of them returning the `ErrDuplicate` sentinel. Because the tasks finish in nondeterministic order, the collected errors arrive in nondeterministic order too, so the demo sorts the report lines before printing: a stable, reproducible report is something you want in real diagnostics anyway. It also calls `errors.Is` to prove the sentinel is found even though it was not the first failure.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"example.com/collect-all"
)

func main() {
	tasks := []collect.Task{
		{Name: "validate-email", Fn: func() error { time.Sleep(8 * time.Millisecond); return errors.New("invalid format") }},
		{Name: "validate-age", Fn: func() error { time.Sleep(4 * time.Millisecond); return errors.New("must be positive") }},
		{Name: "validate-name", Fn: func() error { return nil }},
		{Name: "check-duplicate", Fn: func() error { time.Sleep(2 * time.Millisecond); return collect.ErrDuplicate }},
		{Name: "check-quota", Fn: func() error { return nil }},
	}

	err := collect.RunAll(tasks, 3)
	if err == nil {
		fmt.Println("all checks passed")
		return
	}

	// errors.Is reaches every collected cause, not just the first to fail.
	fmt.Println("duplicate among failures:", errors.Is(err, collect.ErrDuplicate))

	lines := strings.Split(err.Error(), "\n")
	slices.Sort(lines) // completion order is nondeterministic; sort for a stable report
	fmt.Printf("%d checks failed:\n", len(lines))
	for _, line := range lines {
		fmt.Println("  -", line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
duplicate among failures: true
3 checks failed:
  - check-duplicate: duplicate entry
  - validate-age: must be positive
  - validate-email: invalid format
```

The two passing checks (`validate-name`, `check-quota`) never appear; `Add` dropped their `nil`. The sorted order is alphabetical by line, which is why `check-duplicate` leads regardless of which task actually finished first.

### Tests

The tests pin the contract. `TestAddIgnoresNil` proves a nil error is not recorded. `TestAddWrapsWithLabel` proves the label is prepended and the chain is preserved (`errors.Is` reaches the cause). `TestErrMatchesEveryCause` is the lesson's main test: it adds three errors and asserts `errors.Is` finds both the first and the last, which only holds because `errors.Join` exposes the whole slice. `TestConcurrentAddIsRaceFree` hammers `Add` from 100 goroutines under `-race`. `TestRunAllCollectsAllFailures` and `TestRunAllAllSucceed` pin the runner.

Create `collector_test.go`:

```go
package collect

import (
	"errors"
	"sync"
	"testing"
)

func TestAddIgnoresNil(t *testing.T) {
	t.Parallel()
	var c Collector
	c.Add("task", nil)
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
	if c.Err() != nil {
		t.Fatalf("Err = %v, want nil", c.Err())
	}
}

func TestAddWrapsWithLabel(t *testing.T) {
	t.Parallel()
	base := errors.New("bad")
	var c Collector
	c.Add("validate", base)
	if !errors.Is(c.Err(), base) {
		t.Fatal("Err should match the wrapped cause via errors.Is")
	}
	if got := c.Err().Error(); got != "validate: bad" {
		t.Fatalf("Err = %q, want %q", got, "validate: bad")
	}
}

func TestErrMatchesEveryCause(t *testing.T) {
	t.Parallel()
	first := errors.New("first")
	last := errors.New("last")
	var c Collector
	c.Add("a", first)
	c.Add("b", errors.New("middle"))
	c.Add("c", last)
	if !errors.Is(c.Err(), first) || !errors.Is(c.Err(), last) {
		t.Fatal("joined error must match every collected cause, not only the first")
	}
}

func TestConcurrentAddIsRaceFree(t *testing.T) {
	t.Parallel()
	var c Collector
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				c.Add("task", errors.New("boom"))
			} else {
				c.Add("task", nil)
			}
		}()
	}
	wg.Wait()
	if c.Len() != 50 {
		t.Fatalf("Len = %d, want 50", c.Len())
	}
}

func TestRunAllCollectsAllFailures(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	tasks := []Task{
		{Name: "ok", Fn: func() error { return nil }},
		{Name: "f1", Fn: func() error { return errors.New("e1") }},
		{Name: "f2", Fn: func() error { return sentinel }},
	}
	err := RunAll(tasks, 2)
	if err == nil {
		t.Fatal("RunAll should return the collected failures")
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("RunAll error should match a sentinel returned by any task")
	}
}

func TestRunAllAllSucceed(t *testing.T) {
	t.Parallel()
	tasks := []Task{
		{Name: "a", Fn: func() error { return nil }},
		{Name: "b", Fn: func() error { return nil }},
	}
	if err := RunAll(tasks, 1); err != nil {
		t.Fatalf("RunAll = %v, want nil", err)
	}
}
```

## Review

The collector is correct when three properties hold together. `Add` ignores nil, so a caller never has to guard the call; it wraps with `%w`, so the label is human-readable and the chain is machine-readable; and `Err` uses `errors.Join`, so `errors.Is` and `errors.As` reach every cause. The third is the one most implementations get wrong: an aggregator that exposes only the first error's chain — a single-valued `Unwrap` returning one error — passes a test that searches for the first sentinel and silently fails one that searches for a later one. `TestErrMatchesEveryCause` exists precisely to catch that regression. Confirm the mutex guards every path: `Add` writes under the lock, and `Len` and `Err` read under it, so an iterator can never observe a half-appended slice. The `-race` run on `TestConcurrentAddIsRaceFree` is what proves the locking is real and not decorative.

The common trap is reaching for `errgroup.Wait`'s return value to report failures. `Wait` gives you one error and, under `WithContext`, kills the other tasks before they can report theirs — the exact failures you were trying to collect. The fix is the shape here: tasks return nil to the group, errors go to the collector, and the group is used only to bound and join. The second trap is forgetting that this pattern gives up cancellation; do not use `RunAll` where a failure must stop its siblings.

## Resources

- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — `Group`, `SetLimit`, and the `Wait`-returns-first-error contract this lesson works around.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard aggregator whose `Unwrap` returns `[]error`, which is what makes `errors.Is` reach every cause.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — how `%w`, `errors.Is`, and `errors.As` walk a wrapped error chain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-bulk-import-aggregate-failures.md](02-bulk-import-aggregate-failures.md)
