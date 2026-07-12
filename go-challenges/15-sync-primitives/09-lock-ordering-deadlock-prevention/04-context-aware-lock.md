# Exercise 4: Context-Aware Lock for Request Handlers

`sync.Mutex.Lock` cannot be interrupted: no deadline, no cancellation, no
error. If a request handler blocks on a lock that a wedged or slow goroutine
holds, the handler outlives its own request deadline and the wedge accumulates
goroutines. This exercise builds a lock a handler can wait on *with* its
context, so contention degrades to an observable 503 instead of a silent hang.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
ctxlock/                   independent module: example.com/ctxlock
  go.mod
  ctxlock.go               type Lock (1-buffered chan struct{});
                           New, Acquire(ctx) error, Release
  handler.go               MaintenanceHandler: 503 + Retry-After when
                           Acquire fails on ctx.Done
  cmd/
    demo/
      main.go              runnable demo: acquire, bounded second acquire,
                           release, reacquire
  ctxlock_test.go          deadline-bounded acquire, mutual exclusion under
                           -race, httptest handler tests (200 and 503)
```

- Files: `ctxlock.go`, `handler.go`, `cmd/demo/main.go`, `ctxlock_test.go`.
- Implement: `Lock` backed by a `chan struct{}` of capacity 1 — send to acquire, receive to release — with `Acquire(ctx)` selecting against `ctx.Done()` and `Release` panicking on misuse; an HTTP handler that converts acquisition timeout into 503 with Retry-After.
- Test: acquire-when-free, deadline expiry while held (elapsed bounded), release-then-reacquire, panic on unheld release, handler 200/503 paths via `httptest`, and a guarded-counter mutual-exclusion test under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/04-context-aware-lock/cmd/demo
cd go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/04-context-aware-lock
```

### Preemption: the Coffman condition sync.Mutex cannot break

Ordering (exercise 1) prevents cycles among locks that follow the discipline.
But a request handler often has to take a lock it does not own the design of —
a per-tenant maintenance lock held by a batch job, a migration lock, a lock
that some *other* team's code path can deadlock. When that happens,
`sync.Mutex.Lock` offers exactly one behavior: wait forever. The request's
deadline passes, the client gives up and retries, the retry parks on the same
lock, and the goroutine count graph turns into a ramp.

A channel used as a lock changes the failure mode. A `chan struct{}` with
capacity 1 is a mutex: sending the single token acquires (a second send blocks
because the buffer is full), receiving releases. The crucial difference is that
a channel operation can appear in a `select` — so the wait can race the
request's `ctx.Done()` and give up when the deadline fires. That is a form of
preemption, the fourth Coffman condition: the waiter is preempted by its
deadline. The lock holder is unaffected; what changes is that waiters become
*bounded*, and a wedged holder produces a stream of clean, alertable
`context.DeadlineExceeded` errors instead of an invisible pile of parked
goroutines.

Two details in `Acquire` deserve attention. First, it checks `ctx.Err()`
before selecting: when the context is already cancelled *and* the lock is
free, `select` would choose between the two ready cases pseudo-randomly, and a
cancelled request must not sometimes win a lock. Second, the error returned is
`ctx.Err()` itself — `context.DeadlineExceeded` or `context.Canceled` — so
callers compose with the standard `errors.Is` checks they already use for I/O
timeouts.

`Release` panics if the lock is not held. Misuse — releasing twice, releasing
without acquiring — is a programming error on the same footing as unlocking an
unlocked `sync.Mutex` (which also panics), and silently tolerating it would
corrupt mutual exclusion: the next two acquirers would both succeed.

The trade-offs are real and worth naming: a channel lock is slower than
`sync.Mutex` (a channel operation is heavier than an uncontended atomic CAS),
has no starvation-fairness mode (Go's mutex has one), and no special vet or
runtime integration. Use it at the boundary where cancellability matters — the
request path — and keep `sync.Mutex` for the short internal critical sections
that never block for long.

Create `ctxlock.go`:

```go
// Package ctxlock provides a mutual-exclusion lock whose acquisition
// respects context cancellation and deadlines. It trades the speed and
// fairness of sync.Mutex for the one thing sync.Mutex cannot do: give up
// waiting.
package ctxlock

import "context"

// Lock is a context-aware mutual-exclusion lock. The zero value is not
// usable; call New. The lock is a 1-buffered channel: holding the lock
// means the buffer's single token slot is occupied.
type Lock struct {
	ch chan struct{}
}

// New returns an unheld Lock.
func New() *Lock {
	return &Lock{ch: make(chan struct{}, 1)}
}

// Acquire obtains the lock, blocking until it is free or ctx is done.
// It returns nil on success and ctx.Err() otherwise, so callers can use
// errors.Is(err, context.DeadlineExceeded) as they would for I/O.
func (l *Lock) Acquire(ctx context.Context) error {
	// A context that is already done must never win the lock, even if the
	// lock is free: select chooses among ready cases pseudo-randomly.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees the lock. It panics if the lock is not held — the same
// contract as unlocking an unlocked sync.Mutex, because tolerating it
// would let two later acquirers both succeed.
func (l *Lock) Release() {
	select {
	case <-l.ch:
	default:
		panic("ctxlock: Release of unheld Lock")
	}
}
```

### The handler: contention becomes a 503, not a hang

The handler bounds its wait with `context.WithTimeout` layered on the request
context, so it honors *both* the server-side budget and client disconnection.
On failure it sets `Retry-After` — the standard header that tells well-behaved
clients and load balancers when to try again — and returns 503, the status
that means "temporarily unable, not broken". The alternative behaviors are all
worse: blocking forever (goroutine leak), returning 500 (pages the on-call for
what is really backpressure), or dropping the connection (client retries
immediately, amplifying load).

Create `handler.go`:

```go
package ctxlock

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// MaintenanceHandler serves work that must not run while the tenant's
// maintenance lock is held elsewhere. It waits up to maxWait for the
// lock; on timeout it degrades to 503 with a Retry-After header instead
// of hanging past the request deadline.
func MaintenanceHandler(l *Lock, maxWait time.Duration, work func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), maxWait)
		defer cancel()

		if err := l.Acquire(ctx); err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "tenant locked for maintenance, retry later", http.StatusServiceUnavailable)
			return
		}
		defer l.Release()

		fmt.Fprintln(w, work())
	})
}
```

### The runnable demo

The demo runs entirely on one goroutine, which is exactly the point: a
`sync.Mutex` version of "acquire while held" on one goroutine would deadlock
forever, while `Acquire` returns with an error after its 50 ms budget.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/ctxlock"
)

func main() {
	l := ctxlock.New()

	if err := l.Acquire(context.Background()); err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Println("first acquire: ok")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); err != nil {
		fmt.Println("second acquire while held:", err)
	}

	l.Release()
	if err := l.Acquire(context.Background()); err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Println("after release: acquired again")
	l.Release()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first acquire: ok
second acquire while held: context deadline exceeded
after release: acquired again
```

### Tests

The deadline test asserts two things: the error is `context.DeadlineExceeded`
(via `errors.Is`), and the call returned in bounded time — the elapsed-time
ceiling is generous to survive CI scheduling noise, but it would catch an
`Acquire` that ignores the context entirely. The mutual-exclusion test is the
race-detector workout: 20 goroutines increment and decrement a deliberately
non-atomic counter inside the critical section; if two ever hold the lock at
once, the counter check fails or `-race` reports the write race. Handler tests
use `httptest.NewRecorder` — no real server — and pin both the 200 path and
the 503 + Retry-After path. Note `t.Context()` (Go 1.24+) as the parent
context throughout: it is cancelled automatically when the test ends, so a
leaked waiter cannot outlive its test.

Create `ctxlock_test.go`:

```go
package ctxlock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireWhenFree(t *testing.T) {
	t.Parallel()

	l := New()
	if err := l.Acquire(t.Context()); err != nil {
		t.Fatalf("Acquire on free lock: %v", err)
	}
	l.Release()
}

func TestAcquireDeadlineWhileHeld(t *testing.T) {
	t.Parallel()

	l := New()
	if err := l.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := l.Acquire(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Acquire blocked %v; the deadline should bound it", elapsed)
	}
}

func TestAcquireAlreadyCancelled(t *testing.T) {
	t.Parallel()

	l := New() // free — but a dead context must still not win it
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestReleaseThenAcquire(t *testing.T) {
	t.Parallel()

	l := New()
	if err := l.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	l.Release()
	if err := l.Acquire(t.Context()); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	l.Release()
}

func TestReleaseUnheldPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("Release of unheld lock did not panic")
		}
	}()
	New().Release()
}

func TestMutualExclusion(t *testing.T) {
	t.Parallel()

	l := New()
	var inside int // deliberately non-atomic: the lock must protect it
	var wg sync.WaitGroup

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if err := l.Acquire(t.Context()); err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				inside++
				if inside != 1 {
					t.Errorf("mutual exclusion violated: inside = %d", inside)
				}
				inside--
				l.Release()
			}
		}()
	}
	wg.Wait()
}

func TestHandlerServesWhenFree(t *testing.T) {
	t.Parallel()

	l := New()
	h := MaintenanceHandler(l, 100*time.Millisecond, func() string { return "report ready" })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/report", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "report ready" {
		t.Errorf("body = %q, want %q", got, "report ready")
	}
}

func TestHandler503WhenHeld(t *testing.T) {
	t.Parallel()

	l := New()
	if err := l.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer l.Release() // simulate a long-running maintenance job

	h := MaintenanceHandler(l, 30*time.Millisecond, func() string { return "unreachable" })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/report", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
}

func ExampleLock_Acquire() {
	l := New()
	if err := l.Acquire(context.Background()); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println("held")
	l.Release()
	// Output: held
}
```

## Review

The whole design hangs on one channel invariant: a buffered channel of
capacity 1 admits exactly one in-flight token, so "token in buffer" is
isomorphic to "lock held", and both transitions (send, receive) are
select-able. Everything else is contract hygiene: the early `ctx.Err()` check
keeps cancelled requests from winning a free lock through select's random
choice; returning `ctx.Err()` keeps the error vocabulary standard; the
panicking `Release` matches `sync.Mutex.Unlock` semantics because a tolerated
double-release quietly destroys mutual exclusion for everyone after.

Keep the tool in its lane. This lock makes *waiters* cancellable; it does
nothing about a holder that never releases — pair it with the watchdog testing
from exercise 9 to catch those. And do not replace every internal `sync.Mutex`
with it: for a critical section of nanoseconds the channel's overhead and lack
of starvation-fairness are a bad trade, and the race detector's mutex-aware
diagnostics are worth keeping. The boundary where requests wait on
long-possibly-held locks is where this earns its cost. Confirm with
`go test -count=1 -race ./...`.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`, `Done`, and the `Err` contract `Acquire` forwards.
- [Effective Go — channels](https://go.dev/doc/effective_go#channels) — buffered channels as semaphores, the idiom this lock is built on.
- [testing package — T.Context](https://pkg.go.dev/testing#T.Context) — the per-test context used as the parent throughout the tests.
- [MDN: Retry-After](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Retry-After) — the header contract for 503 backpressure responses.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-lock-hierarchy-debug-mutex.md](05-lock-hierarchy-debug-mutex.md)
