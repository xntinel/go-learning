# Exercise 9: A Leak Guard That Registers First and Runs Last

"No resource leaked" should be an enforced invariant, not a hope. LIFO cleanup
ordering makes that possible: register a leak guard *first* in the fixture and, by
last-in-first-out, its cleanup runs *last* — after every resource has been
released — so it can assert the live-handle count returned to zero.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
leakguard/                   independent module: example.com/leakguard
  go.mod                     go 1.24
  client.go                  Client with a live-handle counter; Handle with idempotent Close
  cmd/
    demo/
      main.go                runnable demo: open two handles, close them, watch the count
  client_test.go             newTestClient(t) registers the guard FIRST so it runs LAST
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: a `Client` whose `Open` increments a live-handle `atomic.Int64` and a `Handle` whose `Close` decrements it exactly once.
- Test: a `guardNoResourceLeaks` cleanup registered first (so LIFO runs it last) asserting the count is zero; a demonstration that the guard's logic detects a real leak.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Registration order is the whole trick

Cleanups run last-registered, first-called. The leak guard is a cleanup that reads
the live-handle count and fails if it is not zero. For that assertion to be
meaningful it must run *after* every resource cleanup has released its handle — so
the guard must be *registered first*, before any resource opens. LIFO then places
it last in the teardown sequence. Register the guard last instead and it runs
first, before any handle closes, sees a non-zero count, and false-fails. The
ordering is not a detail; it is the mechanism.

Put both halves in the fixture. `newTestClient(t)` builds the client and
immediately calls `guardNoResourceLeaks(t, c)`, which registers the guard cleanup —
first, so it runs last. Every subsequent `openHandle(t, c)` opens a handle and
registers *its* close as a later cleanup, so it runs before the guard. At test end
the sequence is: all the handle closes fire (LIFO, most recent first), the count
falls to zero, and finally the guard runs and confirms it. "No leaks" is now an
invariant the fixture enforces on every test that uses it.

Track leaks with your *own* counter, an `atomic.Int64` the client owns, not by
snapshotting `runtime.NumGoroutine()`. `NumGoroutine` is racy against the scheduler
and against other packages' tests, so a leak check built on it flakes. An explicit
counter that `Open` increments and `Close` decrements is deterministic: it reflects
exactly your fixture's handles and nothing else. `Close` must be idempotent so a
double close cannot drive the count negative — a `CompareAndSwap` on a `closed`
flag guarantees the decrement happens exactly once.

Create `client.go`:

```go
package leakguard

import "sync/atomic"

// Client is a fake backend client that hands out Handles and tracks how many are
// currently live, so a test can assert none leaked.
type Client struct {
	live atomic.Int64
}

// NewClient returns a client with no live handles.
func NewClient() *Client { return &Client{} }

// Open returns a new live handle and increments the live-handle count.
func (c *Client) Open() *Handle {
	c.live.Add(1)
	return &Handle{client: c}
}

// Live reports how many handles are currently open.
func (c *Client) Live() int64 { return c.live.Load() }

// Handle is a per-open resource. Close is idempotent.
type Handle struct {
	client *Client
	closed atomic.Bool
}

// Close releases the handle exactly once, decrementing the live count. A second
// call is a no-op, so it can never drive the count negative.
func (h *Handle) Close() {
	if h.closed.CompareAndSwap(false, true) {
		h.client.live.Add(-1)
	}
}
```

### The runnable demo

The demo opens two handles and closes them one at a time, printing the live count
so you can watch it rise and fall.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/leakguard"
)

func main() {
	c := leakguard.NewClient()
	h1 := c.Open()
	h2 := c.Open()
	fmt.Printf("live after 2 opens: %d\n", c.Live())

	h1.Close()
	fmt.Printf("live after 1 close: %d\n", c.Live())

	h2.Close()
	fmt.Printf("live after 2 closes: %d\n", c.Live())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
live after 2 opens: 2
live after 1 close: 1
live after 2 closes: 0
```

### The tests

`newTestClient` registers the guard first; `openHandle` opens a handle and
registers its close. `TestNoLeakWhenAllClosed` opens four handles through the
fixture and passes because the guard, running last, sees zero live handles.
`TestGuardRunsLastByLIFO` records cleanup order to show the guard registered first
runs last. `TestGuardDetectsRealLeak` opens a handle *without* registering its
close inside a subtest and, after the subtest's guard cleanup has run, confirms the
guard observed the leak — proving its detection is real without failing the suite.

Create `client_test.go`:

```go
package leakguard

import (
	"fmt"
	"slices"
	"testing"
)

// newTestClient builds a client and registers the leak guard FIRST, so by LIFO
// the guard's cleanup runs LAST — after every handle close.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	c := NewClient()
	guardNoResourceLeaks(t, c)
	return c
}

// guardNoResourceLeaks registers a cleanup asserting no handle stayed live.
func guardNoResourceLeaks(t *testing.T, c *Client) {
	t.Helper()
	t.Cleanup(func() {
		if n := c.Live(); n != 0 {
			t.Errorf("resource leak: %d handle(s) still live at test end", n)
		}
	})
}

// openHandle opens a handle and registers its close, so the handle is released
// before the guard (registered earlier) checks the count.
func openHandle(t *testing.T, c *Client) *Handle {
	t.Helper()
	h := c.Open()
	t.Cleanup(h.Close)
	return h
}

func TestNoLeakWhenAllClosed(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	for range 4 {
		openHandle(t, c)
	}
	if got := c.Live(); got != 4 {
		t.Fatalf("live = %d while open, want 4", got)
	}
	// At test end: the four handle closes run first (LIFO), the count falls to
	// zero, and the guard (registered first) runs last and confirms it.
}

func TestGuardRunsLastByLIFO(t *testing.T) {
	t.Parallel()
	var order []string
	t.Run("layered", func(t *testing.T) {
		// Registered in order guard, handleA, handleB; LIFO runs them in reverse.
		t.Cleanup(func() { order = append(order, "guard") })
		t.Cleanup(func() { order = append(order, "handleA") })
		t.Cleanup(func() { order = append(order, "handleB") })
	})
	want := []string{"handleB", "handleA", "guard"}
	if !slices.Equal(order, want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
}

func TestGuardDetectsRealLeak(t *testing.T) {
	t.Parallel()
	c := NewClient()
	var leakDetected bool
	t.Run("subtest that leaks", func(t *testing.T) {
		// Guard registered first => runs last, after any resource cleanup.
		t.Cleanup(func() {
			if c.Live() != 0 {
				leakDetected = true
			}
		})
		// Open a handle but deliberately register no close: a leak.
		_ = c.Open()
	})
	if !leakDetected {
		t.Fatal("guard failed to detect the leaked handle")
	}
	if c.Live() != 1 {
		t.Fatalf("live = %d, want 1 (the leaked handle)", c.Live())
	}
}

func TestHandleCloseIdempotent(t *testing.T) {
	t.Parallel()
	c := NewClient()
	h := c.Open()
	h.Close()
	h.Close()
	if got := c.Live(); got != 0 {
		t.Fatalf("live = %d after double close, want 0", got)
	}
}

func ExampleClient() {
	c := NewClient()
	h := c.Open()
	fmt.Println(c.Live())
	h.Close()
	fmt.Println(c.Live())
	// Output:
	// 1
	// 0
}
```

## Review

The guard is correct when it is registered before any resource and asserts a zero
live-handle count. `TestNoLeakWhenAllClosed` passes because LIFO runs the four
handle closes before the guard; `TestGuardRunsLastByLIFO` pins that ordering
directly; and `TestGuardDetectsRealLeak` proves the guard's condition actually
fires on a leaked handle, without failing the real suite. The mistakes to avoid:
never register the guard last and expect it to run first — LIFO makes it run
before the resources release and false-fail; and never build leak detection on
`runtime.NumGoroutine()`, which is racy against the scheduler and other tests —
track your own handles with an `atomic.Int64`. Keep `Close` idempotent via
`CompareAndSwap` so a double close cannot drive the count negative. Run
`go test -race` to confirm the atomic counter is sound under the parallel subtests.

## Resources

- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — registration order and LIFO execution, the basis of the first-registered-runs-last guard.
- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — the race-free live-handle counter.
- [`sync/atomic.Bool`](https://pkg.go.dev/sync/atomic#Bool) — `CompareAndSwap` makes `Close` idempotent.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — comparing the recorded cleanup order.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-setenv-chdir-serial-config.md](08-setenv-chdir-serial-config.md) | Next: [10-testmain-suite-lifecycle.md](10-testmain-suite-lifecycle.md)
