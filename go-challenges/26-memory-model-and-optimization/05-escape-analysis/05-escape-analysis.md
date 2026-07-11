# 5. Escape Analysis

Escape analysis is the compiler's proof about object lifetime. This lesson builds a small batching package that keeps reusable storage under caller control, validates configuration with sentinel errors, and gives you code you can inspect with `-gcflags=-m` without turning the exercise into a non-testable `main` program.

```text
escapeplan/
  go.mod
  batch.go
  batch_test.go
  cmd/demo/main.go
```

## Concepts

### Stack Or Heap Is A Compiler Decision

Go programmers do not choose stack or heap directly. The compiler decides whether a value can stay within the current stack frame or must live longer. If the compiler can prove the value does not outlive the function, the value can stay on the stack. If a pointer to the value is returned, stored somewhere that may outlive the call, captured by an escaping closure, or passed to code that may retain it, the value can escape to the heap.

### Escape Output Explains Why, Not Just Where

`go build -gcflags=-m` prints decisions such as moved values, inlining, and whether arguments escape. The exact wording changes across Go versions, so tests should not assert compiler diagnostics. Use the diagnostics to guide a refactor, then use normal tests and benchmarks to prove the public behavior still holds.

### Caller-Owned Storage Reduces Allocation Pressure

Returning a new slice on every hot-path call is simple but often allocates. A common alternative is to let the caller provide a destination slice and return the used prefix. The caller controls retention and reuse, while the callee keeps validation and transformation logic in one place.

### Do Not Fight Readability For A Hypothetical Escape

Escape analysis is an optimization aid, not a reason to make every API pointer-free. If a pointer return is the correct ownership model, keep it. Optimize only after profiling or after a code path is known to be allocation-sensitive.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/escapeplan/cmd/demo
cd ~/go-exercises/escapeplan
go mod init escapeplan
```

This is a library package. The demo is only a consumer of the exported API; `go test` is the verification mechanism.

### Exercise 1: Implement The Batching API

Create `batch.go`:

```go
package escapeplan

import (
	"errors"
	"fmt"
)

var (
	ErrNilEvents      = errors.New("events must not be nil")
	ErrInvalidLimit   = errors.New("limit must be positive")
	ErrCapacityTooLow = errors.New("destination capacity is too low")
)

type Event struct {
	ID    int
	Topic string
}

type Batch struct {
	IDs []int
}

func NewBatch(events []Event, limit int) (Batch, error) {
	if events == nil {
		return Batch{}, fmt.Errorf("new batch: %w", ErrNilEvents)
	}
	if limit <= 0 {
		return Batch{}, fmt.Errorf("new batch: %w: got %d", ErrInvalidLimit, limit)
	}
	if limit > len(events) {
		limit = len(events)
	}

	ids := make([]int, 0, limit)
	for i := 0; i < limit; i++ {
		ids = append(ids, events[i].ID)
	}
	return Batch{IDs: ids}, nil
}

func FillBatch(dst []int, events []Event, limit int) ([]int, error) {
	if events == nil {
		return nil, fmt.Errorf("fill batch: %w", ErrNilEvents)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("fill batch: %w: got %d", ErrInvalidLimit, limit)
	}
	if limit > len(events) {
		limit = len(events)
	}
	if cap(dst) < limit {
		return nil, fmt.Errorf("fill batch: %w: cap=%d limit=%d", ErrCapacityTooLow, cap(dst), limit)
	}

	dst = dst[:0]
	for i := 0; i < limit; i++ {
		dst = append(dst, events[i].ID)
	}
	return dst, nil
}

func CountTopic(events []Event, topic string) int {
	count := 0
	for _, event := range events {
		if event.Topic == topic {
			count++
		}
	}
	return count
}
```

`NewBatch` owns the result slice. `FillBatch` reuses caller-owned storage and is the version you inspect when you want to reduce allocations in a hot loop.

### Exercise 2: Test Validation And Reuse

Create `batch_test.go`:

```go
package escapeplan

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestNewBatch(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: 10, Topic: "audit"}, {ID: 20, Topic: "billing"}}
	tests := []struct {
		name  string
		limit int
		want  []int
	}{
		{name: "one", limit: 1, want: []int{10}},
		{name: "clamps to length", limit: 5, want: []int{10, 20}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			batch, err := NewBatch(events, tt.limit)
			if err != nil {
				t.Fatalf("NewBatch() error = %v", err)
			}
			if !reflect.DeepEqual(batch.IDs, tt.want) {
				t.Fatalf("IDs = %v, want %v", batch.IDs, tt.want)
			}
		})
	}
}

func TestFillBatchReusesDestination(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: 1}, {ID: 2}, {ID: 3}}
	dst := make([]int, 0, 3)
	got, err := FillBatch(dst, events, 2)
	if err != nil {
		t.Fatalf("FillBatch() error = %v", err)
	}
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("got = %v", got)
	}
	if cap(got) != cap(dst) {
		t.Fatalf("cap(got) = %d, want %d", cap(got), cap(dst))
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	events := []Event{{ID: 1}}
	tests := []struct {
		name string
		fn   func() error
		want error
	}{
		{name: "new nil events", fn: func() error { _, err := NewBatch(nil, 1); return err }, want: ErrNilEvents},
		{name: "new invalid limit", fn: func() error { _, err := NewBatch(events, 0); return err }, want: ErrInvalidLimit},
		{name: "fill nil events", fn: func() error { _, err := FillBatch(make([]int, 0, 1), nil, 1); return err }, want: ErrNilEvents},
		{name: "fill invalid limit", fn: func() error { _, err := FillBatch(make([]int, 0, 1), events, -1); return err }, want: ErrInvalidLimit},
		{name: "fill capacity", fn: func() error { _, err := FillBatch(make([]int, 0, 0), events, 1); return err }, want: ErrCapacityTooLow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.fn(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCountTopic(t *testing.T) {
	t.Parallel()

	events := []Event{{Topic: "audit"}, {Topic: "billing"}, {Topic: "audit"}}
	if got := CountTopic(events, "audit"); got != 2 {
		t.Fatalf("CountTopic() = %d, want 2", got)
	}
}

func ExampleFillBatch() {
	events := []Event{{ID: 7}, {ID: 8}, {ID: 9}}
	dst := make([]int, 0, len(events))
	ids, _ := FillBatch(dst, events, 2)
	fmt.Println(ids)
	// Output: [7 8]
}
```

The tests assert behavior with table-driven cases and use `errors.Is` against wrapped sentinel errors. The example is also verified by `go test`.

### Exercise 3: Add The Exported-API Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"escapeplan"
)

func main() {
	events := []escapeplan.Event{
		{ID: 101, Topic: "audit"},
		{ID: 102, Topic: "billing"},
		{ID: 103, Topic: "audit"},
	}

	ids, err := escapeplan.FillBatch(make([]int, 0, len(events)), events, 2)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ids=%v audit=%d\n", ids, escapeplan.CountTopic(events, "audit"))
}
```

From the module root, inspect escape decisions with:

```bash
go build -gcflags=-m ./...
```

Treat that command as diagnostic output, not as the pass/fail check.

## Common Mistakes

### Assuming A Pointer Always Means Faster

Wrong: returning `*Batch` only because copying a small struct looks expensive.

Fix: return `Batch` by value when it represents ownership cleanly. In this lesson `Batch` is only a slice header, so copying the struct is cheap; the underlying slice allocation is the important part.

### Testing Compiler Messages Instead Of Behavior

Wrong: writing tests that match exact `-gcflags=-m` text.

Fix: test behavior and run compiler diagnostics separately. Escape messages are useful, but they are not a stable API.

### Reusing Caller Storage Without Checking Capacity

Wrong: `dst = dst[:limit]` before confirming that `cap(dst) >= limit`.

Fix: return `ErrCapacityTooLow` wrapped with context. The caller can allocate once at the right size and reuse safely.

## Verification

Run this from `~/go-exercises/escapeplan`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Then run `go build -gcflags=-m ./...` and compare the compiler's allocation notes for `NewBatch` and `FillBatch`. Add one more test that calls `FillBatch` with a limit larger than the event slice and proves it clamps to `len(events)`.

## Summary

- Escape analysis decides object lifetime from proof, not from syntax alone.
- `-gcflags=-m` is a diagnostic tool; the exact output is not a test contract.
- Caller-owned destination slices can reduce repeated allocations in hot paths.
- Validation errors should stay testable with sentinel errors and `errors.Is`.

## What's Next

Next: [Struct Field Ordering and Cache Lines](../06-struct-field-ordering-cache-lines/06-struct-field-ordering-cache-lines.md).

## Resources

- [Go FAQ: How do I know whether a variable is allocated on the heap or the stack?](https://go.dev/doc/faq#stack_or_heap)
- [cmd/compile: compiler flags](https://pkg.go.dev/cmd/compile)
- [Go Blog: Profiling Go Programs](https://go.dev/blog/pprof)
