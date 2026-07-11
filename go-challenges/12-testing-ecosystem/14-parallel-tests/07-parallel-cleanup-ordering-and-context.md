# Exercise 7: LIFO Teardown and Context Cancellation Across Parallel Subtests

Integration-style tests build fixtures in layers: a temp directory, a store rooted
in it, a background worker writing to the store. Those layers must unwind in
reverse — stop the worker, then close the store, then delete the directory — and
the unwind must happen only after all parallel subtests finish. This module proves
`t.Cleanup`'s LIFO order and uses `t.Context()` to drain the worker gracefully
just before teardown.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
fixturestack/               independent module: example.com/fixturestack
  go.mod
  store.go                  Store (append-only file); Worker (ctx-driven drain)
  cmd/
    demo/
      main.go               runnable demo: submit, cancel, drain, count lines
  store_test.go             layered t.Cleanup proving LIFO + graceful drain
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: an append-only `Store` and a `Worker` started with a context that
drains and exits when the context is cancelled.
Test: register a `t.Cleanup` per fixture layer, assert teardown is strict LIFO,
and assert the worker observes `t.Context()` cancellation before its cleanup
returns.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/fixturestack/cmd/demo
cd ~/go-exercises/fixturestack
go mod init example.com/fixturestack
```

### Why LIFO, and how to observe it

Fixtures have dependencies: the store's file lives inside the temp dir, the worker
writes to the store. So teardown must run in the reverse of construction —
worker, then store, then dir — or you delete the directory out from under an open
file, or close the store while the worker is still writing to it. `t.Cleanup`
gives you exactly this for free: it runs registered functions last-added-first-
called. Register a cleanup right after building each layer and the unwind is
automatically correct.

To *prove* the order in a test, record a label in each layer's cleanup, and
register an assertion cleanup *first* — so, by LIFO, it runs *last* and observes
the complete recorded sequence. The recorded order must be `[worker, store,
tempdir]`: the worker cleanup was registered last so it fires first, and so on.

### Why t.Context() for the worker

The worker runs a goroutine; the test must both stop it and wait for it to finish
its in-flight work before the store is closed. `t.Context()` returns a context
that the testing framework cancels *just before* the test's cleanup functions run.
That timing is precisely what you want: by the time the worker's cleanup executes,
the context is already cancelled, so the worker has begun draining; the cleanup
calls `worker.Wait()` to join the drained goroutine, and only then does the *next*
cleanup (store close) run. You get "cancel, drain, then tear down" ordering without
threading a manual `cancel` func through the test. The worker's cleanup runs before
the store's because it was registered later — the same LIFO rule, now doing real
graceful-shutdown work.

The parallel subtests submit work inside a `t.Run("group", ...)` wrapper, so all
of them complete before the parent function returns and the cleanup cascade begins.

Create `store.go`:

```go
package fixturestack

import (
	"bufio"
	"context"
	"os"
	"sync"
	"sync/atomic"
)

// Store is an append-only line store backed by a file. It is safe for concurrent
// Append calls.
type Store struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// OpenStore opens (creating if needed) an append-only store at path.
func OpenStore(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Store{f: f, w: bufio.NewWriter(f)}, nil
}

// Append writes one line.
func (s *Store) Append(line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.WriteString(line + "\n")
	return err
}

// Close flushes and closes the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	return s.f.Close()
}

// Worker consumes submitted lines and appends them to a Store. It drains and
// exits when its context is cancelled.
type Worker struct {
	in      chan string
	done    chan struct{}
	drained atomic.Bool
}

// StartWorker launches a background goroutine bound to ctx. When ctx is
// cancelled the worker drains any buffered submissions, records that it saw the
// cancellation, and exits.
func StartWorker(ctx context.Context, store *Store) *Worker {
	w := &Worker{in: make(chan string, 64), done: make(chan struct{})}
	go func() {
		for {
			select {
			case line := <-w.in:
				_ = store.Append(line)
			case <-ctx.Done():
				for {
					select {
					case line := <-w.in:
						_ = store.Append(line)
					default:
						w.drained.Store(true)
						close(w.done)
						return
					}
				}
			}
		}
	}()
	return w
}

// Submit queues a line for the worker.
func (w *Worker) Submit(line string) { w.in <- line }

// Wait blocks until the worker has drained and exited.
func (w *Worker) Wait() { <-w.done }

// SawCancel reports whether the worker exited via context cancellation.
func (w *Worker) SawCancel() bool { return w.drained.Load() }
```

### The runnable demo

The demo builds the stack, submits three lines, cancels, waits for the drain, and
counts the lines the store persisted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"example.com/fixturestack"
)

func main() {
	dir, _ := os.MkdirTemp("", "fixture")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "data.log")

	store, err := fixturestack.OpenStore(path)
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	worker := fixturestack.StartWorker(ctx, store)
	for _, line := range []string{"a", "b", "c"} {
		worker.Submit(line)
	}
	cancel()
	worker.Wait()
	_ = store.Close()

	fmt.Printf("saw cancel: %v\n", worker.SawCancel())
	fmt.Printf("lines persisted: %d\n", countLines(path))
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	return n
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
saw cancel: true
lines persisted: 3
```

### Tests

The test records teardown labels and asserts the strict LIFO order. The assertion
cleanup is registered first (runs last), then `tempdir`, `store`, and `worker`
cleanups. The worker cleanup calls `worker.Wait()` — which returns only after the
worker drained in response to `t.Context()` cancellation — and asserts
`SawCancel()`. Parallel subtests submit inside the Run-group, so they all finish
before the cleanup cascade.

Create `store_test.go`:

```go
package fixturestack

import (
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

func TestFixtureStackTeardown(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(label string) {
		mu.Lock()
		order = append(order, label)
		mu.Unlock()
	}

	// Registered FIRST -> runs LAST: sees the full teardown sequence.
	t.Cleanup(func() {
		want := []string{"worker", "store", "tempdir"}
		if !slices.Equal(order, want) {
			t.Errorf("teardown order = %v, want %v", order, want)
		}
	})

	dir := t.TempDir()
	t.Cleanup(func() { record("tempdir") })

	store, err := OpenStore(filepath.Join(dir, "data.log"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
		record("store")
	})

	// t.Context() is cancelled just before cleanups run, so the worker drains
	// before its cleanup (registered last -> runs first) joins it.
	worker := StartWorker(t.Context(), store)
	t.Cleanup(func() {
		worker.Wait()
		if !worker.SawCancel() {
			t.Error("worker did not observe context cancellation before teardown")
		}
		record("worker")
	})

	// Run-group: all parallel submitters finish before the parent returns.
	t.Run("group", func(t *testing.T) {
		for i := range 8 {
			t.Run("submit", func(t *testing.T) {
				t.Parallel()
				worker.Submit("line-" + strconv.Itoa(i))
			})
		}
	})
}

func TestStoreAppend(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "s.log")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for i := range 3 {
		if err := store.Append("row-" + strconv.Itoa(i)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
```

## Review

The teardown is correct when the recorded order is exactly `[worker, store,
tempdir]` — the reverse of construction — because each layer's cleanup was
registered after the layer it depends on, and `t.Cleanup` runs LIFO. The worker's
cleanup fires first and joins the goroutine that drained in response to
`t.Context()` cancellation, so the store is still open while the worker finishes
writing; only then does the store's cleanup close it. That is graceful-shutdown-
before-teardown, expressed purely through registration order.

The trap this avoids is a parent-level `defer store.Close()` with parallel
submitters: it would close the store before the paused subtests resume and submit,
losing writes or erroring. `t.Cleanup` (and the Run-group) delay all teardown
until every subtest completes.

## Resources

- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — LIFO cleanup after a test and its subtests finish.
- [`testing.T.Context`](https://pkg.go.dev/testing#T.Context) — a context cancelled just before cleanups run.
- [`testing.T.TempDir`](https://pkg.go.dev/testing#T.TempDir) — an auto-removed per-test temp directory.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-bounded-parallelism-scarce-pool.md](06-bounded-parallelism-scarce-pool.md) | Next: [08-flaky-detection-shuffle-count-race.md](08-flaky-detection-shuffle-count-race.md)
