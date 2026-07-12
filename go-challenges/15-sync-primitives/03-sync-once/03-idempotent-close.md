# Exercise 3: An io.Closer whose Close runs cleanup exactly once

Graceful shutdown is where idempotent teardown earns its keep. Many in-flight
request goroutines may all reach for the same resource's `Close()` at once, and a
correct `Close` must run the real teardown exactly once, return the same error to
every caller, and reject writes afterward. This exercise builds a buffered
`io.Closer` around `sync.Once` that does exactly that.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
idempotent-close/             module: example.com/idempotent-close
  go.mod
  resource.go                 ErrClosed; type Resource; Write, Close, Teardowns
  cmd/
    demo/
      main.go                 runnable demo: write, close, double close, write-after-close
  resource_test.go            close-once, stable error, 100-goroutine close, write-after-close
```

- Files: `resource.go`, `cmd/demo/main.go`, `resource_test.go`.
- Implement: a `Resource` wrapping a sink `io.Writer`; `Write` buffers unless closed (then `ErrClosed`); `Close` flushes to the sink exactly once via `sync.Once`, records the flush error, and returns that same error on every call; `Teardowns()` proves the flush ran once.
- Test: two `Close` calls run teardown once and return the identical error value; a failing sink yields a stable wrapped error across calls; 100 concurrent `Close` calls run teardown once; a `Write` after `Close` returns `ErrClosed`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/03-sync-once/03-idempotent-close/cmd/demo
cd go-solutions/15-sync-primitives/03-sync-once/03-idempotent-close
```

### Why Once is the right guard for Close

The naive idempotent close guards teardown with a `bool`: "if closed, return;
else set closed and flush." Under concurrency that check-and-set is a data race,
and worse, two goroutines can both see `closed == false` and both flush. `Once`
solves both at a stroke: the flush body runs exactly once, the internal
double-checked lock makes the guard atomic, and the memory-model edge publishes
the recorded `closeErr` to every caller. So `Close` becomes: run the flush inside
`once.Do`, storing the flush error and marking the resource closed; then return
the stored error. The second, hundredth, and thousandth `Close` all skip the body
and return the identical stored error value — stability that callers can rely on
(they can compare `err == firstErr`, not merely `errors.Is`).

Writes after close must be rejected, and this is where `atomic.Bool` complements
`Once`. The flush body `Store`s `closed = true`; `Write` checks `closed.Load()`
lock-free on the hot path and returns `ErrClosed` if set. We do not want `Write`
to take the same mutex the buffer uses if it can cheaply short-circuit on the
atomic first. The buffer itself is guarded by a plain `Mutex` because `Write`
appends to it and the flush reads it; those must not race.

Create `resource.go`:

```go
package resource

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by Write after the Resource has been closed.
var ErrClosed = errors.New("resource: use of closed resource")

// Resource buffers writes and flushes them to sink exactly once on Close. Close
// is safe to call any number of times from any goroutine; every call returns the
// same flush error. Hold it behind a pointer.
type Resource struct {
	mu        sync.Mutex
	buf       []byte
	sink      io.Writer
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
	teardowns atomic.Int64
}

// New returns a Resource that flushes its buffer to sink on Close.
func New(sink io.Writer) *Resource {
	return &Resource{sink: sink}
}

// Write appends p to the buffer. After Close it returns ErrClosed.
func (r *Resource) Write(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under the lock: a concurrent Close may have flipped closed
	// between our atomic load and acquiring the lock.
	if r.closed.Load() {
		return 0, ErrClosed
	}
	r.buf = append(r.buf, p...)
	return len(p), nil
}

// Close flushes the buffer to the sink exactly once and returns the flush error.
// Every subsequent call returns that same error without flushing again.
func (r *Resource) Close() error {
	r.closeOnce.Do(func() {
		r.teardowns.Add(1)
		r.mu.Lock()
		data := r.buf
		r.mu.Unlock()
		_, err := r.sink.Write(data)
		r.closeErr = err
		r.closed.Store(true)
	})
	return r.closeErr
}

// Teardowns reports how many times the flush body ran; it must be 0 or 1.
func (r *Resource) Teardowns() int64 {
	return r.teardowns.Load()
}
```

There is a deliberate ordering detail in `Close`: it stores `closeErr` before
`closed`, and `Write` re-checks `closed` under the mutex. Once `closed` is true,
no further `Write` can append, so the flushed `data` snapshot is final. The
happens-before edge from `Do` publishes `closeErr` to every later `Close` caller.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"

	"example.com/idempotent-close"
)

func main() {
	var sink bytes.Buffer
	r := resource.New(&sink)

	fmt.Fprintf(r, "line one\n")
	fmt.Fprintf(r, "line two\n")

	fmt.Println("close 1:", r.Close())
	fmt.Println("close 2:", r.Close()) // no-op, same error
	fmt.Println("teardowns:", r.Teardowns())
	fmt.Printf("flushed: %q\n", sink.String())

	_, err := r.Write([]byte("late"))
	fmt.Println("write after close:", errors.Is(err, resource.ErrClosed))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
close 1: <nil>
close 2: <nil>
teardowns: 1
flushed: "line one\nline two\n"
write after close: true
```

### Tests

`TestCloseFlushesOnce` closes twice and asserts one teardown. `TestCloseErrorStable`
uses a sink that always fails and asserts both `Close` calls return the identical
error value (not just `errors.Is`-equal). `TestConcurrentClose` fans 100 goroutines
at `Close` and asserts a single teardown under `-race`. `TestWriteAfterClose`
checks the `ErrClosed` path.

Create `resource_test.go`:

```go
package resource

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

// failWriter is a sink whose Write always fails with errFlush.
var errFlush = errors.New("sink flush failed")

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errFlush }

func TestCloseFlushesOnce(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	r := New(&sink)
	if _, err := r.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := r.Teardowns(); got != 1 {
		t.Fatalf("Teardowns() = %d, want 1", got)
	}
	if got := sink.String(); got != "hello" {
		t.Fatalf("flushed %q, want hello", got)
	}
}

func TestCloseErrorStable(t *testing.T) {
	t.Parallel()

	r := New(failWriter{})
	first := r.Close()
	second := r.Close()
	if !errors.Is(first, errFlush) {
		t.Fatalf("first Close = %v, want errFlush", first)
	}
	if first != second {
		t.Fatalf("Close returned different errors: %v vs %v", first, second)
	}
	if got := r.Teardowns(); got != 1 {
		t.Fatalf("Teardowns() = %d, want 1", got)
	}
}

func TestConcurrentClose(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	r := New(&sink)
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = r.Close()
		}()
	}
	wg.Wait()

	if got := r.Teardowns(); got != 1 {
		t.Fatalf("Teardowns() = %d, want exactly 1", got)
	}
}

func TestWriteAfterClose(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	r := New(&sink)
	_ = r.Close()
	if _, err := r.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
}
```

## Review

The resource is correct when teardown is exactly-once and the returned error is
stable. `TestCloseFlushesOnce` and `TestConcurrentClose` prove one flush across
one and one hundred callers; `TestCloseErrorStable` proves every caller gets the
identical error value, which is the property that makes a `Once`-guarded `Close`
usable as an `io.Closer` in shutdown code. The trap this guards against is the
bare-`bool` idempotent close, whose check-and-set is itself a race and can flush
twice; `Once` makes the guard atomic. Note the double-checked `closed` in `Write`:
the atomic fast path avoids the mutex for the common post-close reject, and the
re-check under the lock closes the window against a concurrent `Close`. Run
`go test -race`.

## Resources

- [io.Closer — pkg.go.dev](https://pkg.go.dev/io#Closer)
- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [Go Code Review Comments: Don't panic / error handling](https://go.dev/wiki/CodeReviewComments)

---

Prev: [02-lazy-counter-start.md](02-lazy-counter-start.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-lazy-pool-provider.md](04-lazy-pool-provider.md)
