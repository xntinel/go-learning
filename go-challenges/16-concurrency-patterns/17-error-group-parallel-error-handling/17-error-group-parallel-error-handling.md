# 17. Collect All Errors From Parallel Tasks

`errgroup.Group.Wait()` returns one error. Sometimes you need all of them: a validator that reports every malformed field, a health check that surfaces every failed dependency, a batch of independent operations where each failure should be visible. The pattern is a mutex-protected slice of errors, a `MultiError` that wraps each entry with the task name and `%w`, and a runner that lets every task run to completion. `errors.Join` (Go 1.20+) is the stdlib primitive for the final aggregated message; the lesson adds the per-task annotation that `errors.Join` does not do on its own.

```text
multi/
  go.mod
  internal/multierror/multierror.go
  internal/multierror/multierror_test.go
  cmd/multidemo/main.go
```

The package exposes a `MultiError` and a `RunAll` runner. The `cmd/multidemo` CLI runs five validation tasks and prints the per-task list and the combined message. The captured output is the lesson's documentation.

## Concepts

### `errgroup.Wait` Returns One Error

`errgroup.Group.Wait()` returns the first non-nil error it observes. The pattern is correct when "any failure aborts" is the right contract (a migration, a deploy). It is wrong when the caller needs a complete report.

### The Pattern: Every Task Runs, Every Error Is Recorded

The alternative pattern is:

1. Each task runs to completion (or to its own context deadline). It is not cancelled because a sibling failed.
2. Each error is recorded with a task label and `%w` (so `errors.Is` and `errors.As` can walk the chain).
3. The runner returns a `MultiError` that aggregates all of them.
4. The caller iterates `Errors()` and decides what to do with each.

The trade-off: a failing task can no longer cancel its siblings. The pattern fits validation, health checks, and batch diagnostics, not critical-path work.

### `errors.Join` Is The Stdlib Aggregator

`errors.Join(errs...)` returns an `error` whose `Error()` is the newline-joined messages of all non-nil inputs. The lesson's `MultiError.Error()` is line-for-line equivalent to `errors.Join(me.errs...).Error()`. The lesson keeps a custom type because the per-task annotation (the `Add(task, err)` shape) is the lesson's actual contribution; the joined-message part is stdlib.

### `errors.Is` And `errors.As` Walk The Chain

Each entry in `MultiError` is `fmt.Errorf("%s: %w", task, err)`. `errors.Is(merr, sentinel)` walks the chain because `MultiError.Unwrap()` returns the first entry's underlying error. The test for the lesson proves this end-to-end.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/multi/internal/multierror ~/go-exercises/multi/cmd/multidemo
cd ~/go-exercises/multi
go mod init example.com/multi
```

### Exercise 1: Implement The MultiError

Create `internal/multierror/multierror.go`:

```go
package multierror

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type MultiError struct {
	mu   sync.Mutex
	errs []error
}

func (me *MultiError) Add(task string, err error) {
	if err == nil {
		return
	}
	me.mu.Lock()
	defer me.mu.Unlock()
	me.errs = append(me.errs, fmt.Errorf("%s: %w", task, err))
}

func (me *MultiError) HasErrors() bool {
	me.mu.Lock()
	defer me.mu.Unlock()
	return len(me.errs) > 0
}

func (me *MultiError) Errors() []error {
	me.mu.Lock()
	defer me.mu.Unlock()
	cp := make([]error, len(me.errs))
	copy(cp, me.errs)
	return cp
}

// Error returns a newline-joined message of all the collected errors.
func (me *MultiError) Error() string {
	me.mu.Lock()
	defer me.mu.Unlock()
	if len(me.errs) == 0 {
		return ""
	}
	msgs := make([]string, len(me.errs))
	for i, e := range me.errs {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "\n")
}

// Unwrap returns the first error's underlying cause. errors.Is/As on a
// MultiError walks the chain via this hook.
func (me *MultiError) Unwrap() error {
	me.mu.Lock()
	defer me.mu.Unlock()
	if len(me.errs) == 0 {
		return nil
	}
	return errors.Unwrap(me.errs[0])
}
```

`Add` is the only mutator; it ignores `nil` and wraps the error with the task name. `Error` is the joined message. `Unwrap` is what makes `errors.Is` and `errors.As` work on a `*MultiError`.

### Exercise 2: Test The MultiError

Create `internal/multierror/multierror_test.go`:

```go
package multierror

import (
	"errors"
	"fmt"
	"testing"
)

func TestEmptyHasNoErrors(t *testing.T) {
	t.Parallel()
	var me MultiError
	if me.HasErrors() {
		t.Fatal("empty MultiError should not have errors")
	}
	if me.Error() != "" {
		t.Fatalf("Error = %q, want empty", me.Error())
	}
}

func TestAddIgnoresNil(t *testing.T) {
	t.Parallel()
	var me MultiError
	me.Add("task", nil)
	if me.HasErrors() {
		t.Fatal("Add(nil) should not record an error")
	}
}

func TestAddRecordsErrorWithTaskPrefix(t *testing.T) {
	t.Parallel()
	base := errors.New("bad format")
	var me MultiError
	me.Add("validate-email", base)
	errs := me.Errors()
	if len(errs) != 1 {
		t.Fatalf("len = %d, want 1", len(errs))
	}
	if !errors.Is(errs[0], base) {
		t.Fatalf("errs[0] = %v, want wrap of base", errs[0])
	}
	if got := errs[0].Error(); got != "validate-email: bad format" {
		t.Fatalf("Error = %q, want %q", got, "validate-email: bad format")
	}
}

func TestErrorsReturnsCopy(t *testing.T) {
	t.Parallel()
	var me MultiError
	me.Add("a", errors.New("x"))
	got := me.Errors()
	got[0] = errors.New("mutated")
	if me.Errors()[0].Error() == "mutated" {
		t.Fatal("Errors should return a copy; mutation leaked back")
	}
}

func TestErrorIsWalksChain(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	wrapped := fmt.Errorf("layer: %w", sentinel)
	var me MultiError
	me.Add("task", wrapped)
	if !errors.Is(&me, sentinel) {
		t.Fatal("errors.Is should walk the MultiError chain to the sentinel")
	}
}

func TestUnwrapReturnsFirstUnderlying(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel")
	var me MultiError
	me.Add("a", fmt.Errorf("a: %w", sentinel))
	me.Add("b", errors.New("b-only"))
	if got := errors.Unwrap(&me); got == nil || got.Error() != "a: sentinel" {
		t.Fatalf("Unwrap = %v, want the first underlying error", got)
	}
}
```

`TestErrorIsWalksChain` is the lesson's main test: it proves that an error wrapped in a `*MultiError` is still detectable with `errors.Is` from outside. `TestErrorsReturnsCopy` pins the contract that the slice returned by `Errors()` is a copy (mutating it does not affect the `MultiError`).

### Exercise 3: Run It End To End

Create `cmd/multidemo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/multi/internal/multierror"
)

type Task struct {
	Name string
	Fn   func() error
}

// RunAll runs tasks with bounded parallelism and collects all errors.
// It does not cancel on the first error; every task runs to completion
// and its error is recorded. Use this when you need a complete report
// (e.g. validation, health checks).
func RunAll(tasks []Task, maxConcurrency int) *multierror.MultiError {
	me := &multierror.MultiError{}
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for _, task := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t Task) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := t.Fn(); err != nil {
				me.Add(t.Name, err)
			}
		}(task)
	}

	wg.Wait()
	return me
}

func main() {
	tasks := []Task{
		{"validate-email", func() error {
			time.Sleep(10 * time.Millisecond)
			return fmt.Errorf("invalid email format")
		}},
		{"validate-age", func() error {
			time.Sleep(15 * time.Millisecond)
			return fmt.Errorf("age must be positive")
		}},
		{"validate-name", func() error {
			time.Sleep(5 * time.Millisecond)
			return nil
		}},
		{"check-duplicate", func() error {
			time.Sleep(20 * time.Millisecond)
			return fmt.Errorf("duplicate entry exists")
		}},
		{"check-permissions", func() error {
			time.Sleep(10 * time.Millisecond)
			return nil
		}},
	}

	result := RunAll(tasks, 3)
	if result.HasErrors() {
		fmt.Println("Validation failed:")
		for _, err := range result.Errors() {
			fmt.Printf("  - %v\n", err)
		}
		fmt.Printf("\nCombined:\n%s\n", result.Error())
	} else {
		fmt.Println("All validations passed")
	}
}
```

Run it from `~/go-exercises/multi`:

```bash
go run ./cmd/multidemo
```

Expected output (captured by the author on Go 1.26):

```text
Validation failed:
  - validate-email: invalid email format
  - validate-age: age must be positive
  - check-duplicate: duplicate entry exists

Combined:
validate-email: invalid email format
validate-age: age must be positive
check-duplicate: duplicate entry exists
```

Three errors are reported, in the order the tasks complete (the slowest is the longest, the two fastest are the short ones). The two passing tasks (`validate-name`, `check-permissions`) do not appear in the error list, as expected.

## Common Mistakes

### Cancelling Siblings On First Error

Wrong: a runner that cancels the context on the first error and records only the cancelled tasks. The caller loses the errors that would have surfaced if the siblings had been allowed to run.

Fix: do not cancel. Record every error. The pattern is for diagnostics, not critical-path work. If you need cancellation, use `errgroup`, not `MultiError`.

### Using `errors.Join` Without Per-Task Annotation

Wrong: `errors.Join(errs...)` where each `err` is the raw task error. The aggregated message has the messages but no task name; the caller has to walk the slice to figure out which task failed.

Fix: wrap each error with the task name first, then join: `fmt.Errorf("%s: %w", task, err)`. The `MultiError.Add(task, err)` API enforces this at the type level.

### Returning A Slice Instead Of A Type

Wrong: a runner that returns `[]error` from `RunAll`. The caller has to do `if len(errs) > 0`; the type is a slice, not an error. The aggregation has to happen at the call site.

Fix: return a `*MultiError` that satisfies `error`. The caller writes `if err := RunAll(...); err != nil { ... }` like any other error. The slice is exposed via `Errors()` for iteration, but the error contract is one value.

### Sharing The Slice Without A Mutex

Wrong: a `MultiError` whose internal slice is appended to from multiple goroutines without synchronization. The race detector catches it on the first run; production catches it on the second.

Fix: the mutex is the only mutator. Read paths (`Error`, `Errors`, `HasErrors`, `Unwrap`) take the mutex too, so the iterators are consistent with the appenders.

## Verification

Run this from `~/go-exercises/multi`:

```bash
test -z "$(gofmt -l .)"
go test -count=1 -race ./...
go vet ./...
go build ./...
go run ./cmd/multidemo
```

`go build ./...` proves the `cmd/multidemo` binary compiles. The `go run` step produces the captured output above. The test suite pins the contract: empty, nil, recording with prefix, copy-on-read, `errors.Is` walks the chain, `Unwrap` returns the first underlying.

The optional "swap the bounded parallelism for an errgroup" exercise (not in the tests) is left to the reader: the per-task error collection is the contribution of the lesson, the bounded-parallelism runner is a thin wrapper over a channel semaphore. Either errgroup with its limit option or the channel semaphore in this lesson gives the same shape; the lesson's `MultiError` is what `RunAll` returns.

## Summary

- `errgroup.Wait` returns one error. Sometimes you need all of them. Use `MultiError`.
- The pattern: every task runs to completion; every error is recorded with a task label and `%w`.
- `errors.Join` is the stdlib aggregator; the lesson adds per-task annotation.
- `errors.Is` and `errors.As` walk the chain via `MultiError.Unwrap()`.
- This pattern is for diagnostics (validation, health checks), not critical-path work.

## What's Next

Next: [Bounded Worker Pool with Adaptive Sizing](../18-bounded-worker-pool-adaptive-sizing/18-bounded-worker-pool-adaptive-sizing.md).

## Resources

- [errors package](https://pkg.go.dev/errors)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
