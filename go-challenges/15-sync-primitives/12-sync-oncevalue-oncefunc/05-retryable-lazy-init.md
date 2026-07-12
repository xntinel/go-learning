# Exercise 5: Retryable Lazy Init: When OnceValues Is the Wrong Tool

The previous exercise proved that `OnceValues` bricks a process when the
first connect attempt hits a transient outage; this exercise builds the tool
that pattern actually needs — a generic `Lazy[T]` that retries failed
initialization on the next call and only latches on success.

## What you'll build

```text
retrylazy/                 independent module: example.com/retrylazy
  go.mod                   go mod init example.com/retrylazy
  retrylazy.go             type Lazy[T any]; New, Get(ctx) (T, error), Done()
  retrylazy_test.go        fresh-error-per-attempt, latch-on-success, 50-goroutine single-init, Example
  cmd/
    demo/
      main.go              runnable demo: broker connect fails twice, succeeds, then cached
```

- Files: `retrylazy.go`, `retrylazy_test.go`, `cmd/demo/main.go`.
- Implement: `Lazy[T any]` with `New[T](init func(ctx context.Context) (T, error)) *Lazy[T]`, `Get(ctx) (T, error)` that re-runs init after failure and latches only on success, and `Done() bool`.
- Test: init fails N times then succeeds — each failing `Get` returns that attempt's fresh error, attempt N+1 succeeds, no init runs after success; 50 concurrent `Get` calls execute init exactly once, under `-race`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/12-sync-oncevalue-oncefunc/05-retryable-lazy-init/cmd/demo
cd go-solutions/15-sync-primitives/12-sync-oncevalue-oncefunc/05-retryable-lazy-init
```

### Why Once cannot express this

`sync.Once` and its helpers have exactly one bit of state: not-done or done.
A failed init has to land on one side of that bit, and both sides are wrong
for transient failures. If failure counts as done (which is what
`OnceValues` does), the error is permanent. If failure did not count as done,
`Once` would have to re-run `f` — but nothing in its API distinguishes
"f returned an error" from "f succeeded", because `Once.Do` takes a
`func()` with no results. The retry-until-success semantics require a third
state signal ("tried and failed, try again"), and that means building on a
mutex, where *we* decide what latches.

The design is small and worth reading as a checklist of decisions:

- A `sync.Mutex` serializes attempts. Concurrent callers during an init
  attempt **wait** rather than stampede — the single-flight property is
  preserved. When a dependency is down, one goroutine at a time probes it;
  fifty goroutines do not pile fifty dials onto a struggling broker.
- `done` latches only on success. Every failed attempt returns *that
  attempt's* error, fresh — not a stale cached one — so callers and logs see
  the dependency's current story ("attempt 3: connection refused" rather than
  attempt 1's error forever).
- After success the fast path is mutex-acquire, flag-check, return. If your
  read path is hot enough that even an uncontended mutex hurts, the upgrade
  is the double-checked pattern with `atomic.Pointer[T]` in front — measure
  before paying that complexity.
- The context is checked before starting an attempt and passed into `init`
  so a dial can honor cancellation and deadlines.

One trade-off must be stated plainly, because it is the price of
single-flight-with-mutex: while one caller's `init` runs, other callers block
in `Lock()`, and **a blocked `Lock` cannot observe their contexts**. A
caller with a 50 ms deadline that arrives while another caller's 5 s dial is
in flight will wait out the dial and only then see its expired context. If
your service cannot accept that, you need a channel-based gate or
`golang.org/x/sync/singleflight` with per-caller wait cancellation — real
added complexity, which is exactly why this trade-off should be a documented
decision rather than an accident. Bound the damage by making `init` itself
enforce a hard timeout on its work.

Create `retrylazy.go`:

```go
// Package retrylazy provides a lazily initialized value whose init function
// is retried on the next Get after a failure and latched only on success --
// the semantics sync.OnceValues cannot express, needed for lazily connecting
// to dependencies (broker, downstream API) that may be temporarily down.
package retrylazy

import (
	"context"
	"sync"
)

// Lazy holds a value produced by init on first successful Get. Failed
// attempts are not cached: the next Get retries. The zero value is not
// usable; construct with New and share by pointer.
type Lazy[T any] struct {
	mu   sync.Mutex
	init func(ctx context.Context) (T, error)
	val  T
	done bool
}

// New returns a Lazy whose Get runs init until one call succeeds.
func New[T any](init func(ctx context.Context) (T, error)) *Lazy[T] {
	return &Lazy[T]{init: init}
}

// Get returns the initialized value. If no previous attempt has succeeded,
// it runs init; on failure it returns that attempt's error and leaves the
// Lazy ready to retry. Concurrent callers during an attempt wait on the
// mutex (single-flight; note a blocked caller cannot observe its context
// until the in-flight attempt finishes).
func (l *Lazy[T]) Get(ctx context.Context) (T, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return l.val, nil
	}
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	v, err := l.init(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	l.val, l.done = v, true
	return v, nil
}

// Done reports whether a previous Get succeeded. Like Loaded in the counter
// exercise, it observes without triggering initialization.
func (l *Lazy[T]) Done() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done
}
```

### The demo

The demo plays the broker-connect scenario `OnceValues` fails at: the
dependency refuses the first two dials and accepts the third. Watch the error
*change* between attempts (fresh, not cached), the success latch on attempt
3, and call 4 served from the latched value with no further dials.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/retrylazy"
)

type broker struct{ addr string }

func main() {
	dials := 0
	conn := retrylazy.New(func(ctx context.Context) (*broker, error) {
		dials++
		if dials <= 2 {
			return nil, fmt.Errorf("dial amqp://broker:5672: attempt %d: connection refused", dials)
		}
		return &broker{addr: "amqp://broker:5672"}, nil
	})

	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		b, err := conn.Get(ctx)
		if err != nil {
			fmt.Printf("call %d: %v\n", i, err)
			continue
		}
		fmt.Printf("call %d: connected to %s\n", i, b.addr)
	}
	fmt.Println("dials:", dials)
	fmt.Println("latched:", conn.Done())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: dial amqp://broker:5672: attempt 1: connection refused
call 2: dial amqp://broker:5672: attempt 2: connection refused
call 3: connected to amqp://broker:5672
call 4: connected to amqp://broker:5672
dials: 3
latched: true
```

Compare line by line with the demo of exercise 4: same failure pattern in the
dependency, opposite outcome in the process.

### Tests

`TestRetriesUntilSuccess` is the core contract and deliberately asserts the
error is *fresh* each attempt — each failure carries its own attempt number —
then that success latches and the init never runs again.
`TestConcurrentSingleInit` proves single-flight: 50 goroutines on a cold
`Lazy` with a slow-ish init produce exactly one init execution.
`TestContextCancelled` pins that a dead context fails fast without consuming
an attempt.

Create `retrylazy_test.go`:

```go
package retrylazy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errUnavailable = errors.New("unavailable")

func TestRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	const failures = 2
	var attempts atomic.Int64
	l := New(func(ctx context.Context) (string, error) {
		n := attempts.Add(1)
		if n <= failures {
			return "", fmt.Errorf("attempt %d: %w", n, errUnavailable)
		}
		return "conn", nil
	})

	ctx := t.Context()
	for i := 1; i <= failures; i++ {
		_, err := l.Get(ctx)
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("attempt %d: err = %v, want errUnavailable", i, err)
		}
		if want := fmt.Sprintf("attempt %d:", i); !strings.Contains(err.Error(), want) {
			t.Fatalf("attempt %d returned stale error %q, want fresh %q", i, err, want)
		}
		if l.Done() {
			t.Fatalf("Done() = true after failed attempt %d", i)
		}
	}

	v, err := l.Get(ctx)
	if err != nil || v != "conn" {
		t.Fatalf("attempt %d: got %q, %v; want conn, nil", failures+1, v, err)
	}
	if !l.Done() {
		t.Fatal("Done() = false after success")
	}

	for range 10 {
		if v, err := l.Get(ctx); err != nil || v != "conn" {
			t.Fatalf("post-success Get = %q, %v", v, err)
		}
	}
	if got := attempts.Load(); got != failures+1 {
		t.Fatalf("init ran %d times, want %d (never again after success)", got, failures+1)
	}
}

func TestConcurrentSingleInit(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	l := New(func(ctx context.Context) (int, error) {
		attempts.Add(1)
		time.Sleep(10 * time.Millisecond) // let the other 49 goroutines pile up
		return 42, nil
	})

	ctx := t.Context()
	var wg sync.WaitGroup
	const goroutines = 50
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			v, err := l.Get(ctx)
			if err != nil || v != 42 {
				t.Errorf("Get = %d, %v; want 42, nil", v, err)
			}
		}()
	}
	wg.Wait()
	if got := attempts.Load(); got != 1 {
		t.Fatalf("init ran %d times under concurrency, want 1", got)
	}
}

func TestContextCancelled(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	l := New(func(ctx context.Context) (int, error) {
		attempts.Add(1)
		return 1, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with dead context = %v, want context.Canceled", err)
	}
	if got := attempts.Load(); got != 0 {
		t.Fatalf("init ran %d times despite cancelled context, want 0", got)
	}

	// The Lazy is still usable: a live context succeeds.
	if v, err := l.Get(t.Context()); err != nil || v != 1 {
		t.Fatalf("Get after cancellation = %d, %v; want 1, nil", v, err)
	}
}

func ExampleLazy_Get() {
	attempts := 0
	l := New(func(ctx context.Context) (string, error) {
		attempts++
		if attempts == 1 {
			return "", errors.New("connection refused")
		}
		return "conn", nil
	})
	_, err := l.Get(context.Background())
	fmt.Println("first:", err)
	v, _ := l.Get(context.Background())
	fmt.Println("second:", v)
	// Output:
	// first: connection refused
	// second: conn
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The type earns its place when the tests demonstrate all three behaviors
`OnceValues` cannot give you: fresh errors per failed attempt, retry until
one attempt succeeds, and permanence only after success — plus the
single-flight property those fifty goroutines pin under `-race`. The
judgment call from exercise 4 now has both answers implemented: latch
deterministic failures with `OnceValues`; retry transient ones with this.
The mistakes to watch for in real implementations of this pattern: checking
`done` before taking the mutex (a data race — the flag is mutex-guarded, not
atomic), latching the error as well as the value (which silently rebuilds
`OnceValues`), and forgetting that callers blocked on the mutex cannot see
their contexts — if that matters for your latency budget, say so in the doc
comment as this package does, or reach for singleflight. Note also what this
type does *not* do: it never retries inside a single `Get`. Backoff loops
belong in the caller or in `init` itself, where they can honor the context.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the primitive that lets failure not latch.
- [x/sync singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the heavier tool when blocked callers must honor cancellation.
- [context package](https://pkg.go.dev/context) — Err, cancellation, and deadline propagation into init.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-db-handle-oncevalues.md](04-db-handle-oncevalues.md) | Next: [06-per-key-template-cache.md](06-per-key-template-cache.md)
