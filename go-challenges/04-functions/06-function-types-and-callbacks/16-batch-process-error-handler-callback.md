# Exercise 16: Batch Process Error Handler Callbacks for Fail-Fast vs Collect-All

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch job — importing rows, migrating records, validating a CSV upload —
processes many items and hits some errors along the way. Whether the job
should abort at the first bad item or keep going and report everything wrong
at the end is a policy decision, and it should not be hard-coded into the
loop. This module builds a batch runner parameterized by an
`ErrorHandler` callback that makes that call per item.

## What you'll build

```text
batchproc/                  independent module: example.com/batch-process-error-handler-callback
  go.mod                     go 1.24
  batchproc.go                type ProcessFunc, type ErrorHandler, type Result, func Run, FailFast, CollectAll
  cmd/
    demo/
      main.go                 runnable demo: same batch run under both policies
  batchproc_test.go           table test: fail-fast stop point, collect-all completeness, custom handler
```

Files: `batchproc.go`, `cmd/demo/main.go`, `batchproc_test.go`.
Implement: `type ProcessFunc[T any] func(item T) error`, `type ErrorHandler func(index int, err error) (stop bool)`, a `Result` struct with `Errors`, `StoppedAt`, `Processed`, `func Run[T any](items []T, process ProcessFunc[T], onError ErrorHandler) Result`, and the two stock handlers `FailFast` and `CollectAll`.
Test: fail-fast stops right after the first error and records exactly one, collect-all runs to completion and records every error, a no-error batch behaves identically under either policy, and a custom handler that stops after N errors proves the callback — not the loop — controls the policy.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batch-process-error-handler-callback/cmd/demo
cd ~/go-exercises/batch-process-error-handler-callback
go mod init example.com/batch-process-error-handler-callback
go mod edit -go=1.24
```

### Why the stop decision is a callback, not a boolean flag

The obvious design is a `failFast bool` parameter on `Run`. That works for
exactly two policies. The moment a caller wants "stop after 5 errors" or "stop
only on a specific error type" the boolean has to grow into an enum, then the
enum has to grow branches inside `Run` itself, and the batch runner ends up
knowing about every policy any caller has ever needed. Making the decision a
callback — `ErrorHandler func(index int, err error) (stop bool)` — inverts
that: `Run` stays a fixed, three-line loop, and every policy, however
specific, lives outside it as an ordinary function value. `FailFast` and
`CollectAll` are the two constants everyone needs; a caller with an unusual
policy just writes its own handler with the same signature and passes it in,
with zero changes to `Run`.

Create `batchproc.go`:

```go
package batchproc

// ProcessFunc processes a single item and reports whether it failed.
type ProcessFunc[T any] func(item T) error

// ErrorHandler is called once per failed item during a batch run. It
// receives the item's index and the error that occurred, and reports
// whether the batch should stop processing right there (true) or keep
// going through the remaining items (false).
type ErrorHandler func(index int, err error) (stop bool)

// Result is the outcome of a batch run: which indices failed and with what
// error, plus whether the run was cut short by the ErrorHandler.
type Result struct {
	Errors    map[int]error
	StoppedAt int // index the batch stopped at, or -1 if it ran to completion
	Processed int // number of items actually run through ProcessFunc
}

// Run processes every item with process, in order, and delegates every
// failure to onError. When onError reports stop, Run returns immediately
// without processing the remaining items.
func Run[T any](items []T, process ProcessFunc[T], onError ErrorHandler) Result {
	res := Result{Errors: make(map[int]error), StoppedAt: -1}
	for i, item := range items {
		res.Processed++
		if err := process(item); err != nil {
			res.Errors[i] = err
			if onError(i, err) {
				res.StoppedAt = i
				return res
			}
		}
	}
	return res
}

// FailFast is an ErrorHandler that stops the batch at the very first error.
func FailFast(index int, err error) bool {
	return true
}

// CollectAll is an ErrorHandler that never stops the batch; every error is
// recorded in Result.Errors and processing continues to the last item.
func CollectAll(index int, err error) bool {
	return false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batch-process-error-handler-callback"
)

type order struct {
	ID     string
	Amount int
}

func validate(o order) error {
	if o.Amount <= 0 {
		return fmt.Errorf("order %s: invalid amount %d", o.ID, o.Amount)
	}
	return nil
}

func main() {
	orders := []order{
		{ID: "o1", Amount: 10},
		{ID: "o2", Amount: -5},
		{ID: "o3", Amount: 20},
		{ID: "o4", Amount: 0},
	}

	failFast := batchproc.Run(orders, validate, batchproc.FailFast)
	fmt.Printf("fail-fast: processed=%d stoppedAt=%d errors=%d\n",
		failFast.Processed, failFast.StoppedAt, len(failFast.Errors))

	collectAll := batchproc.Run(orders, validate, batchproc.CollectAll)
	fmt.Printf("collect-all: processed=%d stoppedAt=%d errors=%d\n",
		collectAll.Processed, collectAll.StoppedAt, len(collectAll.Errors))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fail-fast: processed=2 stoppedAt=1 errors=1
collect-all: processed=4 stoppedAt=-1 errors=2
```

### Tests

Create `batchproc_test.go`:

```go
package batchproc

import (
	"errors"
	"testing"
)

func isEven(n int) error {
	if n%2 != 0 {
		return errors.New("odd number")
	}
	return nil
}

func TestRunFailFastStopsAtFirstError(t *testing.T) {
	t.Parallel()
	items := []int{2, 4, 5, 6, 7}
	res := Run(items, isEven, FailFast)

	if res.Processed != 3 {
		t.Fatalf("Processed = %d, want 3", res.Processed)
	}
	if res.StoppedAt != 2 {
		t.Fatalf("StoppedAt = %d, want 2", res.StoppedAt)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("len(Errors) = %d, want 1", len(res.Errors))
	}
	if _, ok := res.Errors[2]; !ok {
		t.Fatalf("expected error recorded at index 2, got %v", res.Errors)
	}
}

func TestRunCollectAllRunsToCompletion(t *testing.T) {
	t.Parallel()
	items := []int{2, 4, 5, 6, 7}
	res := Run(items, isEven, CollectAll)

	if res.Processed != len(items) {
		t.Fatalf("Processed = %d, want %d", res.Processed, len(items))
	}
	if res.StoppedAt != -1 {
		t.Fatalf("StoppedAt = %d, want -1", res.StoppedAt)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("len(Errors) = %d, want 2", len(res.Errors))
	}
	if _, ok := res.Errors[2]; !ok {
		t.Fatalf("expected error at index 2")
	}
	if _, ok := res.Errors[4]; !ok {
		t.Fatalf("expected error at index 4")
	}
}

func TestRunNoErrorsProcessesEverythingEitherPolicy(t *testing.T) {
	t.Parallel()
	items := []int{2, 4, 6, 8}

	for _, handler := range []struct {
		name string
		fn   ErrorHandler
	}{
		{"FailFast", FailFast},
		{"CollectAll", CollectAll},
	} {
		res := Run(items, isEven, handler.fn)
		if res.Processed != len(items) {
			t.Errorf("%s: Processed = %d, want %d", handler.name, res.Processed, len(items))
		}
		if res.StoppedAt != -1 {
			t.Errorf("%s: StoppedAt = %d, want -1", handler.name, res.StoppedAt)
		}
		if len(res.Errors) != 0 {
			t.Errorf("%s: len(Errors) = %d, want 0", handler.name, len(res.Errors))
		}
	}
}

func TestRunCustomHandlerStopsAfterNErrors(t *testing.T) {
	t.Parallel()
	items := []int{1, 1, 1, 1, 2}
	count := 0
	stopAfterTwo := func(index int, err error) bool {
		count++
		return count >= 2
	}
	res := Run(items, isEven, stopAfterTwo)
	if res.StoppedAt != 1 {
		t.Fatalf("StoppedAt = %d, want 1", res.StoppedAt)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("len(Errors) = %d, want 2", len(res.Errors))
	}
}
```

## Review

`Run` never branches on a policy name; it only ever calls `onError` and reads
back one bit. `FailFast` returns `true` unconditionally, so the batch stops
the instant `res.Errors` has one entry — `TestRunFailFastStopsAtFirstError`
pins `Processed == 3` (two clean items plus the one that failed) and
`StoppedAt == 2`. `CollectAll` returns `false` unconditionally, so
`Processed` always reaches `len(items)` and every failing index lands in
`Errors`. The custom "stop after two" handler in the last test is the point
of the whole design: it is a policy `Run` was never told about, expressed
entirely outside `batchproc.go`, in five lines at the call site.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [errors package](https://pkg.go.dev/errors)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-reconciliation-differ-callback.md](15-reconciliation-differ-callback.md) | Next: [17-compression-codec-adapter.md](17-compression-codec-adapter.md)
