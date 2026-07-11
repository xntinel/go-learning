# Exercise 10: Context-aware once getter: waiters can abandon a slow init that Do cannot cancel

`Once.Do` has no context parameter and no escape hatch: no call to `Do` returns
until the function it guards returns. Put a hanging network dial inside the
closure and every request goroutine that touches the getter blocks forever —
the classic init-storm outage. This exercise builds the fix: init still runs
exactly once, but it runs in its own goroutine behind a `done` channel, so
waiters `select` between the result and their own `ctx.Done()`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
context-once-getter/          module: example.com/context-once-getter
  go.mod
  onceget.go                  type Getter[T]; New, Get(ctx), Runs
  cmd/
    demo/
      main.go                 runnable demo: impatient waiter bails, patient
                              waiter gets the value, init ran once
  onceget_test.go             canceled waiter detaches (no sleeps), mixed
                              cancel/wait load, failing init, Example
```

- Files: `onceget.go`, `cmd/demo/main.go`, `onceget_test.go`.
- Implement: a generic `Getter[T]` whose `Get(ctx)` triggers the slow, fallible init exactly once in a dedicated goroutine; the goroutine writes `val`/`err` fields and then closes a `done` channel; every waiter selects on `done` vs `ctx.Done()` and a canceled waiter returns `ctx.Err()` immediately while init continues for later callers.
- Test: a waiter with a canceled context returns `context.Canceled` while init is provably still in flight (gated on a release channel, no sleeps); after release, a fresh `Get` returns the value and the runs counter is 1 across 100 mixed cancel/wait goroutines; a failing init propagates the same captured error to all completing waiters.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p context-once-getter/cmd/demo
cd context-once-getter
go mod init example.com/context-once-getter
```

### Cancellation detaches the waiter, never the work

The design splits `Once`'s two jobs apart. `Do` currently does both *election*
(pick exactly one runner) and *waiting* (block everyone else until the runner
finishes). Election is exactly what you want; the built-in waiting is the
problem, because it is uninterruptible. So the closure passed to `Do` is made
trivially fast: it only *spawns* the init goroutine. `Do` returns immediately
for every caller, and the waiting moves into a `select` that the caller owns:

```
select {
case <-g.done:
	return g.val, g.err
case <-ctx.Done():
	return zero, ctx.Err()
}
```

Two properties fall out, and both must be stated precisely. First, cancellation
is *per waiter, not per work*: a caller whose context fires walks away with
`ctx.Err()`, but the init goroutine keeps running, and its outcome — success or
failure — is still cached for every later caller. Nothing is rolled back. If
your init acquires external resources, a canceled waiter has not released them;
do not treat `ctx.Err()` from `Get` as "init did not happen." (When you need to
know *why* a context died, `context.Cause` gives the cancel cause rather than
the generic `context.Canceled` — worth knowing, though the tests here assert
the standard sentinel.)

Second, the memory-model edge moves from `Do` to the channel. Waiters that
return via `<-g.done` read `g.val` and `g.err` without a lock; that is only
legal because the init goroutine writes both fields *before* `close(g.done)`,
and a close on a channel happens before a receive that completes because of
that close. Reverse the order — close first, assign after — and every waiter's
read races with the writes; the race detector will catch it, and without the
detector you ship torn reads. The `Once` still matters, but for a narrower
reason: it guarantees exactly one init goroutine is ever spawned, so `runs` is
1 no matter how many callers race on the first use.

Compare the failure modes with plain `Once.Do` around a dial with a timeout
inside it: a per-dial timeout bounds how long *everyone* blocks but still
blocks everyone for that long, and it conflates the dial's deadline with each
caller's budget. The channel shape lets a request with a 50 ms budget give up
at 50 ms while a background warmer waits indefinitely — each caller spends its
own budget against the same single init.

Create `onceget.go`:

```go
// Package onceget provides exactly-once lazy initialization whose waiters are
// cancelable: init runs once in its own goroutine, and each Get selects
// between the finished result and its own context.
package onceget

import (
	"context"
	"sync"
	"sync/atomic"
)

// Getter runs a slow, fallible init exactly once and hands the cached result
// to every caller. Waiting on the result honors the caller's context; the
// init itself, once started, always runs to completion.
type Getter[T any] struct {
	init func() (T, error)
	once sync.Once
	done chan struct{}
	val  T
	err  error
	runs atomic.Int64
}

// New returns a Getter around init. Nothing runs until the first Get.
func New[T any](init func() (T, error)) *Getter[T] {
	return &Getter[T]{init: init, done: make(chan struct{})}
}

// Get triggers init on first use and waits for it, honoring ctx. A caller
// whose context ends first returns the zero value and ctx.Err(); init keeps
// running and its outcome is cached for later callers. The val/err fields are
// written before done is closed, so the receive is the happens-before edge
// that makes the field reads race-free.
func (g *Getter[T]) Get(ctx context.Context) (T, error) {
	g.once.Do(func() {
		go func() {
			g.runs.Add(1)
			g.val, g.err = g.init()
			close(g.done)
		}()
	})

	select {
	case <-g.done:
		return g.val, g.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Runs reports how many times init started; it must be 0 or 1.
func (g *Getter[T]) Runs() int64 { return g.runs.Load() }
```

### The runnable demo

The demo models a slow cache dial. An impatient caller with a 50 ms budget
bails with `context.DeadlineExceeded` while the 300 ms init keeps going; a
patient caller then gets the finished connection, and the runs counter shows
one init for both.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	onceget "example.com/context-once-getter"
)

func main() {
	g := onceget.New(func() (string, error) {
		time.Sleep(300 * time.Millisecond) // a slow dial
		return "conn://cache-primary", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.Get(ctx); err != nil {
		fmt.Println("impatient waiter:", err)
	}

	v, err := g.Get(context.Background())
	fmt.Println("patient waiter:", v, err)
	fmt.Println("init runs:", g.Runs())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
impatient waiter: context deadline exceeded
patient waiter: conn://cache-primary <nil>
init runs: 1
```

### Tests

The tests coordinate with channels, never sleeps, so they are deterministic
under `-race`. `TestCanceledWaiterDetaches` holds init open on a `release`
channel, cancels the waiter only after init has provably started, asserts
`context.Canceled`, then releases init and confirms a later `Get` sees the
value with `Runs() == 1`. `TestMixedWaiters` runs 50 pre-canceled and 50
patient waiters against one blocked init. `TestFailingInit` checks the captured
error reaches every completing waiter via `errors.Is`.

Create `onceget_test.go`:

```go
package onceget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestCanceledWaiterDetaches(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	g := New(func() (int, error) {
		close(started)
		<-release
		return 42, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started // init is provably in flight
		cancel()
	}()

	if _, err := g.Get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with canceled ctx = %v, want context.Canceled", err)
	}

	// The abandoned waiter did not cancel the work: release it and read the
	// completed value.
	close(release)
	v, err := g.Get(context.Background())
	if err != nil || v != 42 {
		t.Fatalf("Get after release = %d, %v; want 42, nil", v, err)
	}
	if got := g.Runs(); got != 1 {
		t.Fatalf("Runs() = %d, want exactly 1", got)
	}
}

func TestMixedWaiters(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	g := New(func() (string, error) {
		<-release
		return "ready", nil
	})

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled: these waiters must all detach

	const half = 50
	var wg sync.WaitGroup
	wg.Add(half)
	for range half {
		go func() {
			defer wg.Done()
			if _, err := g.Get(canceledCtx); !errors.Is(err, context.Canceled) {
				t.Errorf("canceled waiter got %v, want context.Canceled", err)
			}
		}()
	}
	wg.Wait() // all 50 detached while init is still blocked

	close(release)

	wg.Add(half)
	for range half {
		go func() {
			defer wg.Done()
			v, err := g.Get(context.Background())
			if err != nil || v != "ready" {
				t.Errorf("patient waiter got %q, %v; want ready, nil", v, err)
			}
		}()
	}
	wg.Wait()

	if got := g.Runs(); got != 1 {
		t.Fatalf("Runs() = %d, want exactly 1 across 100 mixed waiters", got)
	}
}

func TestFailingInit(t *testing.T) {
	t.Parallel()

	errDial := errors.New("dial upstream: connection refused")
	g := New(func() (int, error) {
		return 0, errDial
	})

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := g.Get(context.Background()); !errors.Is(err, errDial) {
				t.Errorf("Get = %v, want the captured init error", err)
			}
		}()
	}
	wg.Wait()

	if got := g.Runs(); got != 1 {
		t.Fatalf("Runs() = %d, want 1 (failure cached, not retried)", got)
	}
}

func Example() {
	g := New(func() (string, error) { return "pool-ready", nil })
	v, err := g.Get(context.Background())
	fmt.Println(v, err)
	// Output: pool-ready <nil>
}
```

## Review

The getter is correct when three things hold: waiters honor their own context
(`context.Canceled` comes back while init is demonstrably still blocked),
exactly-once survives any mix of canceled and patient callers (`Runs() == 1`
across 100 goroutines), and completed results — value or error — reach every
waiter through the `done` receive. The ordering inside the init goroutine is
the part to re-check in review: fields first, `close(done)` last; swapping them
tears the happens-before edge and the `-race` gate will fail. Also hold on to
the semantic caveat: `ctx.Err()` from `Get` means *this caller* gave up, not
that init failed or was rolled back — logging it as an init failure produces
phantom incidents. Like plain `Once`, a failed init is cached forever here; if
you need retry-after-failure, compose this shape with the generational reset
from Exercise 5. Run `go test -count=1 -race`.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [context — pkg.go.dev](https://pkg.go.dev/context)
- [The Go Memory Model: channel communication — go.dev](https://go.dev/ref/mem#chan)

---

Prev: [09-lazy-tls-config.md](09-lazy-tls-config.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../04-sync-map/00-concepts.md](../04-sync-map/00-concepts.md)
