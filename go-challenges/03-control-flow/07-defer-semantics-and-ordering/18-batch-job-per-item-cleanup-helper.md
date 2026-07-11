# Exercise 18: Batch Processor — Per-Item Helper Escapes Defer-in-Loop Trap

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch job that opens a handle per item and defers its close *inside the
loop* registers every one of those defers against the enclosing function,
not the iteration — so on a batch of any real size the process runs out of
file descriptors long before the loop finishes, because none of them close
until the whole batch is done. The fix is not to give up on `defer`; it is
to give the per-item work its own function, so its function boundary — not
the loop's — is where the deferred close actually fires. This module builds
both the correct shape and a test that proves at most one handle is ever
open at once, however many items there are. The module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
batchproc/                  independent module: example.com/batch-job-per-item-cleanup-helper
  go.mod                     go 1.24
  batchproc.go               Tracker, Handle, processItem, ProcessBatch(tracker, items) (int, []error)
  cmd/
    demo/
      main.go                runnable demo: 2000 items with a few bad ones scattered through
  batchproc_test.go          table test over counts; 5000-item peak-open-handle proof
```

- Files: `batchproc.go`, `cmd/demo/main.go`, `batchproc_test.go`.
- Implement: `Tracker` (`OpenHandle`, `Open`, `Peak`), `Handle` (`Close`), `processItem`, `ProcessBatch(tracker *Tracker, items []string) (processed int, errs []error)`.
- Test: a table over item batches asserting processed/error counts, plus a 5000-item run asserting the peak open-handle count never exceeds 1.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batch-job-per-item-cleanup-helper/cmd/demo
cd ~/go-exercises/batch-job-per-item-cleanup-helper
go mod init example.com/batch-job-per-item-cleanup-helper
go mod edit -go=1.24
```

### Why the loop body has to be its own function

The tempting first draft of `ProcessBatch` inlines everything: `for _, item
:= range items { h := tracker.OpenHandle(); defer h.Close(); ... }`. Every
one of those `defer` statements is registered against `ProcessBatch`'s own
frame, because that is the enclosing function — a `for` loop is not a
function boundary, and `defer` only ever fires at *function* return. On
1,000 items that is 1,000 open handles sitting there until `ProcessBatch`
itself finally returns, and on a batch large enough (or a `ulimit` small
enough) the process runs out of file descriptors partway through the loop,
long before any of them get a chance to close. `processItem` fixes this by
existing as its own function: `defer h.Close()` inside it is scoped to
*that call's* return, which happens once per item, right after `processItem`
is done with it. `ProcessBatch`'s loop calls `processItem` once per
iteration; each call's own return is the cleanup boundary, so at most one
handle is ever open regardless of how many items are in the batch.

Create `batchproc.go`:

```go
package batchproc

import "errors"

// ErrEmptyItem means an item in the batch was the empty string.
var ErrEmptyItem = errors.New("batchproc: empty item")

// Tracker stands in for the OS's file descriptor table: it counts how many
// Handles are open right now and remembers the highest count ever reached,
// so a test can prove a batch job never holds more handles open at once
// than the process's ulimit would allow.
type Tracker struct {
	open   int
	peak   int
	nextID int
}

// Open reports the current number of open handles.
func (t *Tracker) Open() int { return t.open }

// Peak reports the highest number of handles ever open at once.
func (t *Tracker) Peak() int { return t.peak }

// OpenHandle simulates opening a file descriptor for one item.
func (t *Tracker) OpenHandle() *Handle {
	t.nextID++
	t.open++
	if t.open > t.peak {
		t.peak = t.open
	}
	return &Handle{id: t.nextID, tracker: t}
}

// Handle simulates a scarce OS resource such as a file descriptor.
type Handle struct {
	id      int
	tracker *Tracker
	closed  bool
}

// Close releases the handle. Safe to call more than once.
func (h *Handle) Close() {
	if h.closed {
		return
	}
	h.closed = true
	h.tracker.open--
}

// processItem opens one handle, does work with it, and closes it via a
// defer scoped to this function's own return. Because processItem is a
// dedicated function rather than inline loop body, its defer fires at the
// end of *this call* -- not at the end of ProcessBatch -- so the handle it
// opens is always closed before ProcessBatch moves to the next item.
func processItem(tracker *Tracker, item string) error {
	h := tracker.OpenHandle()
	defer h.Close()

	if item == "" {
		return ErrEmptyItem
	}
	_ = h.id // stand-in for using the handle to do real work
	return nil
}

// ProcessBatch runs processItem once per item. If processItem instead
// inlined `defer h.Close()` directly in this loop, every one of len(items)
// handles would stay open until ProcessBatch itself returned, exhausting
// file descriptors well before the batch finished on any nontrivial input.
// Extracting processItem as its own function turns the loop body's function
// boundary into the per-iteration cleanup boundary.
func ProcessBatch(tracker *Tracker, items []string) (processed int, errs []error) {
	for _, item := range items {
		if err := processItem(tracker, item); err != nil {
			errs = append(errs, err)
			continue
		}
		processed++
	}
	return processed, errs
}
```

### The runnable demo

The demo processes 2000 items with a bad item scattered every 500 entries,
then prints the peak number of handles that were ever open at once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batch-job-per-item-cleanup-helper"
)

func main() {
	items := make([]string, 2000)
	for i := range items {
		if i%500 == 0 {
			items[i] = "" // a few bad items scattered through the batch
			continue
		}
		items[i] = fmt.Sprintf("item-%d", i)
	}

	tracker := &batchproc.Tracker{}
	processed, errs := batchproc.ProcessBatch(tracker, items)

	fmt.Printf("processed=%d errors=%d\n", processed, len(errs))
	fmt.Printf("peak open handles=%d\n", tracker.Peak())
	fmt.Printf("open handles after batch=%d\n", tracker.Open())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
processed=1996 errors=4
peak open handles=1
open handles after batch=0
```

### Tests

`TestProcessBatchCountsProcessedAndErrors` is a small table over
processed/error counts. `TestProcessBatchNeverExceedsOneOpenHandle` is the
test that actually matters for this exercise: it runs 5000 items through
`ProcessBatch` and asserts `tracker.Peak()` never exceeded `1` — the
property that would fail immediately if `defer h.Close()` were inlined in
the loop instead of scoped inside `processItem`.

Create `batchproc_test.go`:

```go
package batchproc

import (
	"errors"
	"testing"
)

func TestProcessBatchCountsProcessedAndErrors(t *testing.T) {
	tests := []struct {
		name          string
		items         []string
		wantProcessed int
		wantErrs      int
	}{
		{"all valid", []string{"a", "b", "c"}, 3, 0},
		{"one empty item", []string{"a", "", "c"}, 2, 1},
		{"all empty", []string{"", "", ""}, 0, 3},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tracker := &Tracker{}
			processed, errs := ProcessBatch(tracker, tc.items)

			if processed != tc.wantProcessed {
				t.Errorf("processed = %d, want %d", processed, tc.wantProcessed)
			}
			if len(errs) != tc.wantErrs {
				t.Errorf("len(errs) = %d, want %d", len(errs), tc.wantErrs)
			}
			for _, err := range errs {
				if !errors.Is(err, ErrEmptyItem) {
					t.Errorf("err = %v, want %v", err, ErrEmptyItem)
				}
			}
		})
	}
}

func TestProcessBatchNeverExceedsOneOpenHandle(t *testing.T) {
	items := make([]string, 5000)
	for i := range items {
		items[i] = "x"
	}

	tracker := &Tracker{}
	processed, errs := ProcessBatch(tracker, items)

	if processed != len(items) {
		t.Fatalf("processed = %d, want %d", processed, len(items))
	}
	if len(errs) != 0 {
		t.Fatalf("len(errs) = %d, want 0", len(errs))
	}
	if tracker.Peak() != 1 {
		t.Fatalf("peak open handles = %d, want 1 (per-item helper should close before the next iteration opens)", tracker.Peak())
	}
	if tracker.Open() != 0 {
		t.Fatalf("open handles after batch = %d, want 0", tracker.Open())
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The batch job is correct when the number of handles open at any instant
stays bounded by a small constant, regardless of how many items the batch
contains — that is exactly what `TestProcessBatchNeverExceedsOneOpenHandle`
checks. The property holds only because `processItem` is a real function
with its own return, not an inlined loop body: `defer` fires at *function*
return, and giving the per-item work its own function makes that return
happen once per item instead of once for the whole batch. The mistake this
design avoids is the one described in this lesson's concepts file — deferring
resource release directly inside a `for` loop — which passes every test on
small input and then exhausts file descriptors in production the day the
queue backs up past a few hundred items.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a deferred call executes when the *surrounding function* returns.
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — the canonical open/defer-close idiom, and why its scope is a function, not a block.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-request-context-deadline-deferred-cleanup.md](17-request-context-deadline-deferred-cleanup.md) | Next: [19-metrics-aggregator-deferred-flush.md](19-metrics-aggregator-deferred-flush.md)
